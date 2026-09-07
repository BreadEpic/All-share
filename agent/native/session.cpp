// Capture session orchestration and the C ABI for ALL SHARE.
#include "internal.h"

#include <cstring>
#include <algorithm>

namespace allshare {
namespace {

std::atomic<bool> g_initialized{false};
std::mutex g_init_mutex;

// A monitor's refresh rate and scaling, which the client uses to decide how
// fast to ask for frames and whether downscaling would ruin the text.
void QueryDisplayDetail(const DXGI_OUTPUT_DESC& desc, int32_t* refresh_hz, int32_t* scale_percent) {
    *refresh_hz = 60;
    *scale_percent = 100;

    MONITORINFOEXW info{};
    info.cbSize = sizeof(info);
    if (!GetMonitorInfoW(desc.Monitor, &info)) return;

    DEVMODEW mode{};
    mode.dmSize = sizeof(mode);
    if (EnumDisplaySettingsW(info.szDevice, ENUM_CURRENT_SETTINGS, &mode)) {
        if (mode.dmDisplayFrequency > 1) *refresh_hz = static_cast<int32_t>(mode.dmDisplayFrequency);
        // The physical pixel count divided by the coordinate space Windows
        // reports gives the effective scaling, which is how a 150% display is
        // detected without needing a per-monitor DPI awareness context.
        const int32_t logical = desc.DesktopCoordinates.right - desc.DesktopCoordinates.left;
        if (logical > 0 && mode.dmPelsWidth > 0) {
            const int32_t percent = static_cast<int32_t>((mode.dmPelsWidth * 100) / logical);
            if (percent >= 100 && percent <= 400) *scale_percent = percent;
        }
    }
}

}  // namespace

// Session owns one capture thread and everything it touches.
class Session {
public:
    bool Open(const as_open_options& options, std::string* error);
    void Close();

    int32_t NextFrame(int32_t timeout_ms, as_frame* out);
    int32_t NextCursor(as_cursor* out);

    void SetBitrate(int32_t bps);
    void SetFrameRate(int32_t fps);
    int32_t SetResolution(int32_t width, int32_t height);
    int32_t SetMonitor(int32_t monitor_id);
    void SetPreset(int32_t preset);
    void RequestKeyframe();
    void NoteInput(uint32_t sequence);

    void GetInfo(as_info* out);
    void GetStats(as_stats* out);

private:
    void Run();
    bool Rebuild(std::string* error);
    void PublishFrame(EncodedPacket&& packet);
    void PublishCursor(bool shape_changed);

    Device device_;
    Duplicator duplicator_;
    Converter converter_;
    Encoder encoder_;

    std::thread thread_;
    std::atomic<bool> running_{false};

    std::mutex mutex_;
    std::condition_variable frames_ready_;
    std::deque<EncodedPacket> frames_;
    std::deque<as_cursor> cursors_;
    std::vector<std::vector<uint8_t>> cursor_bitmaps_;
    EncodedPacket held_;

    // Requested settings, applied by the capture thread at a frame boundary so
    // the pipeline is never reconfigured mid-encode.
    std::atomic<int32_t> want_bitrate_{0};
    std::atomic<int32_t> want_fps_{60};
    std::atomic<int32_t> want_width_{0};
    std::atomic<int32_t> want_height_{0};
    std::atomic<int32_t> want_monitor_{0};
    std::atomic<int32_t> want_preset_{AS_PRESET_BALANCED};
    std::atomic<bool> want_keyframe_{false};
    std::atomic<bool> geometry_dirty_{false};
    std::atomic<uint32_t> input_sequence_{0};

    as_open_options options_{};
    std::string backend_ = "Desktop Duplication";
    std::string open_error_;

    // Counters, published under the mutex for the stats call.
    int32_t applied_bitrate_ = 0;
    float capture_fps_ = 0, encode_fps_ = 0;
    float capture_ms_ = 0, encode_ms_ = 0, queue_ms_ = 0;
    int32_t idle_skipped_ = 0, dropped_ = 0, reacquires_ = 0;
};

bool Session::Open(const as_open_options& options, std::string* error) {
    options_ = options;
    want_bitrate_.store(options.bitrate > 0 ? options.bitrate : 6'000'000);
    want_fps_.store(options.fps > 0 ? options.fps : 60);
    want_monitor_.store(options.monitor_id);
    want_preset_.store(options.preset);
    want_width_.store(options.width);
    want_height_.store(options.height);

    if (!device_.Create(error)) return false;
    if (!Rebuild(error)) return false;

    running_.store(true);
    thread_ = std::thread([this] { Run(); });
    return true;
}

// Rebuild recreates duplication, conversion and encoding for the current
// monitor and target size. It runs at open and whenever the desktop changes
// underneath us, which Windows does routinely: a resolution change, a UAC
// prompt, a lock, a fast user switch, or a GPU mode change all invalidate a
// duplication and all are recoverable.
bool Session::Rebuild(std::string* error) {
    duplicator_.Reset();
    converter_.Reset();
    encoder_.Reset();

    if (!duplicator_.Create(&device_, want_monitor_.load(), error)) return false;

    int32_t width = want_width_.load();
    int32_t height = want_height_.load();
    if (width <= 0 || height <= 0) {
        width = duplicator_.Width();
        height = duplicator_.Height();
    }
    width = std::max(320, std::min(width, duplicator_.Width()));
    height = std::max(180, std::min(height, duplicator_.Height()));

    if (!converter_.Create(&device_, duplicator_.Width(), duplicator_.Height(),
                           width, height, error)) {
        return false;
    }
    if (!encoder_.Create(&device_, options_.codec, converter_.DestWidth(), converter_.DestHeight(),
                         want_fps_.load(), want_bitrate_.load(), want_preset_.load(), error)) {
        return false;
    }
    geometry_dirty_.store(false);
    want_keyframe_.store(true);
    return true;
}

void Session::Run() {
    // Desktop Duplication and an asynchronous MFT both want a single-threaded
    // apartment they own, so the whole pipeline lives on this thread and Go
    // never calls into Direct3D.
    CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE);

    // The capture thread is pacing a real-time stream; being descheduled behind
    // a background task shows up directly as a stutter the user can see.
    SetThreadPriority(GetCurrentThread(), THREAD_PRIORITY_ABOVE_NORMAL);

    std::string error;
    int64_t window_start = MonotonicMicros();
    int32_t captured_in_window = 0, encoded_in_window = 0;
    int64_t capture_total_us = 0, encode_total_us = 0;
    int64_t next_frame_due_us = MonotonicMicros();
    int consecutive_failures = 0;

    std::vector<EncodedPacket> packets;

    while (running_.load()) {
        if (geometry_dirty_.load()) {
            std::string rebuild_error;
            if (!Rebuild(&rebuild_error)) {
                // A failed reconfiguration must not kill the session: keep the
                // previous pipeline if we can, and retry shortly.
                std::this_thread::sleep_for(std::chrono::milliseconds(200));
                continue;
            }
        }

        // Frame pacing. Sleeping to the deadline rather than after each frame
        // stops small overruns from accumulating into visible drift.
        const int32_t fps = std::max(1, want_fps_.load());
        const int64_t interval_us = 1'000'000LL / fps;
        const int64_t now = MonotonicMicros();
        if (now < next_frame_due_us) {
            const int64_t wait_us = next_frame_due_us - now;
            if (wait_us > 1500) {
                std::this_thread::sleep_for(std::chrono::microseconds(wait_us - 1000));
            }
        }
        next_frame_due_us = std::max(next_frame_due_us + interval_us, MonotonicMicros());

        const int64_t capture_started = MonotonicMicros();
        CapturedFrame frame;
        const int32_t status = duplicator_.Acquire(16, &frame, &error);

        if (status == AS_ERR_LOST) {
            reacquires_++;
            std::string rebuild_error;
            if (!Rebuild(&rebuild_error)) {
                std::this_thread::sleep_for(std::chrono::milliseconds(150));
            }
            continue;
        }
        if (status == AS_ERR_GENERIC || status == AS_ERR_CLOSED) {
            if (++consecutive_failures > 20) break;
            std::this_thread::sleep_for(std::chrono::milliseconds(50));
            continue;
        }
        consecutive_failures = 0;

        if (frame.cursor_changed) PublishCursor(true);

        if (status == AS_TIMEOUT || !frame.desktop_changed || frame.texture == nullptr) {
            // Nothing on screen changed. Not sending anything is the entire
            // point: on a desktop this is most frames, and the bits saved go to
            // the pixels that do change.
            std::lock_guard<std::mutex> lock(mutex_);
            idle_skipped_++;
            continue;
        }

        const int64_t capture_done = MonotonicMicros();
        capture_total_us += capture_done - capture_started;
        captured_in_window++;

        ID3D11Texture2D* nv12 = converter_.Convert(frame.texture, &error);
        if (!nv12) {
            duplicator_.Release();
            geometry_dirty_.store(true);
            continue;
        }

        const bool force_key = want_keyframe_.exchange(false);
        packets.clear();
        if (!encoder_.Encode(nv12, frame.captured_at_us, force_key, &packets, &error)) {
            duplicator_.Release();
            geometry_dirty_.store(true);
            continue;
        }
        duplicator_.Release();

        const int64_t encode_done = MonotonicMicros();
        encode_total_us += encode_done - capture_done;

        const uint32_t sequence = input_sequence_.load();
        for (auto& packet : packets) {
            packet.input_sequence = sequence;
            encoded_in_window++;
            PublishFrame(std::move(packet));
        }

        // A bitrate change is cheap and is applied as soon as it is asked for,
        // unlike a resolution change which costs a keyframe.
        const int32_t bitrate = want_bitrate_.load();
        if (bitrate > 0 && bitrate != applied_bitrate_) {
            encoder_.SetBitrate(bitrate);
            applied_bitrate_ = bitrate;
        }

        const int64_t elapsed = MonotonicMicros() - window_start;
        if (elapsed >= 1'000'000) {
            std::lock_guard<std::mutex> lock(mutex_);
            const double seconds = static_cast<double>(elapsed) / 1'000'000.0;
            capture_fps_ = static_cast<float>(captured_in_window / seconds);
            encode_fps_ = static_cast<float>(encoded_in_window / seconds);
            capture_ms_ = captured_in_window
                ? static_cast<float>(capture_total_us / 1000.0 / captured_in_window) : 0;
            encode_ms_ = encoded_in_window
                ? static_cast<float>(encode_total_us / 1000.0 / encoded_in_window) : 0;
            window_start = MonotonicMicros();
            captured_in_window = encoded_in_window = 0;
            capture_total_us = encode_total_us = 0;
        }
    }

    packets.clear();
    std::string drain_error;
    encoder_.Drain(&packets, &drain_error);
    encoder_.Reset();
    converter_.Reset();
    duplicator_.Reset();
    CoUninitialize();
}

void Session::PublishFrame(EncodedPacket&& packet) {
    std::lock_guard<std::mutex> lock(mutex_);
    // A queue here is pure latency: by the time a backed-up frame could be
    // sent, a newer one already exists. Two deep absorbs a scheduling hiccup
    // and nothing more.
    while (frames_.size() >= 2) {
        frames_.pop_front();
        dropped_++;
    }
    frames_.push_back(std::move(packet));
    frames_ready_.notify_one();
}

void Session::PublishCursor(bool shape_changed) {
    as_cursor update{};
    int32_t x = 0, y = 0;
    bool visible = false;
    duplicator_.CursorPosition(&x, &y, &visible);

    // Positions are normalized against the captured surface so a resolution
    // change cannot misplace the pointer on the client.
    const int32_t surface_w = std::max(1, duplicator_.Width());
    const int32_t surface_h = std::max(1, duplicator_.Height());
    update.x = static_cast<int32_t>(
        (static_cast<int64_t>(std::clamp(x, 0, surface_w - 1)) * 65535) / std::max(1, surface_w - 1));
    update.y = static_cast<int32_t>(
        (static_cast<int64_t>(std::clamp(y, 0, surface_h - 1)) * 65535) / std::max(1, surface_h - 1));
    update.visible = visible ? 1 : 0;

    std::vector<uint8_t> bitmap;
    int32_t width = 0, height = 0, hot_x = 0, hot_y = 0;
    uint32_t shape_id = 0;
    if (shape_changed && duplicator_.TakeCursorShape(&bitmap, &width, &height, &hot_x, &hot_y, &shape_id)) {
        update.shape_id = shape_id;
        update.width = width;
        update.height = height;
        update.hot_x = hot_x;
        update.hot_y = hot_y;
    }

    std::lock_guard<std::mutex> lock(mutex_);
    if (!bitmap.empty()) {
        cursor_bitmaps_.push_back(std::move(bitmap));
        if (cursor_bitmaps_.size() > 16) cursor_bitmaps_.erase(cursor_bitmaps_.begin());
        update.bgra = cursor_bitmaps_.back().data();
    }
    // Only the newest position matters; older ones are already wrong.
    while (cursors_.size() >= 8) cursors_.pop_front();
    cursors_.push_back(update);
}

int32_t Session::NextFrame(int32_t timeout_ms, as_frame* out) {
    std::unique_lock<std::mutex> lock(mutex_);
    if (frames_.empty()) {
        frames_ready_.wait_for(lock, std::chrono::milliseconds(timeout_ms > 0 ? timeout_ms : 1),
                               [this] { return !frames_.empty() || !running_.load(); });
    }
    if (!running_.load() && frames_.empty()) return AS_ERR_CLOSED;
    if (frames_.empty()) return AS_TIMEOUT;

    held_ = std::move(frames_.front());
    frames_.pop_front();
    queue_ms_ = static_cast<float>((UnixMicros() - held_.capture_time_us) / 1000.0);

    out->data = held_.data.data();
    out->size = static_cast<int32_t>(held_.data.size());
    out->keyframe = held_.keyframe ? 1 : 0;
    out->capture_time_us = held_.capture_time_us;
    out->encode_us = held_.encode_us;
    out->width = held_.width;
    out->height = held_.height;
    return AS_OK;
}

int32_t Session::NextCursor(as_cursor* out) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (cursors_.empty()) return 0;
    *out = cursors_.front();
    cursors_.pop_front();
    return 1;
}

void Session::SetBitrate(int32_t bps) {
    if (bps > 0) want_bitrate_.store(bps);
}

void Session::SetFrameRate(int32_t fps) {
    if (fps > 0) want_fps_.store(std::clamp(fps, 1, 240));
}

int32_t Session::SetResolution(int32_t width, int32_t height) {
    if (width < 320 || height < 180) return AS_ERR_GENERIC;
    if (width == converter_.DestWidth() && height == converter_.DestHeight()) return AS_OK;
    want_width_.store(width);
    want_height_.store(height);
    geometry_dirty_.store(true);
    return AS_OK;
}

int32_t Session::SetMonitor(int32_t monitor_id) {
    if (monitor_id == want_monitor_.load()) return AS_OK;
    want_monitor_.store(monitor_id);
    // Switching display means a new native size, so the requested size is
    // cleared and the new monitor's own resolution is adopted.
    want_width_.store(0);
    want_height_.store(0);
    geometry_dirty_.store(true);
    return AS_OK;
}

void Session::SetPreset(int32_t preset) {
    if (preset == want_preset_.load()) return;
    want_preset_.store(preset);
    // The preset changes quantiser limits, which only take effect on a fresh
    // encoder configuration.
    geometry_dirty_.store(true);
}

void Session::RequestKeyframe() { want_keyframe_.store(true); }

void Session::NoteInput(uint32_t sequence) { input_sequence_.store(sequence); }

void Session::GetInfo(as_info* out) {
    std::memset(out, 0, sizeof(*out));
    out->codec = options_.codec;
    out->hardware = encoder_.Hardware() ? 1 : 0;
    out->width = converter_.DestWidth();
    out->height = converter_.DestHeight();
    out->fps = want_fps_.load();
    out->monitor_id = want_monitor_.load();
    out->cursor_embedded = options_.exclude_cursor ? 0 : 1;
    std::snprintf(out->encoder, sizeof(out->encoder), "%s", encoder_.Name().c_str());
    std::snprintf(out->backend, sizeof(out->backend), "%s", backend_.c_str());
    std::snprintf(out->profile, sizeof(out->profile), "%s", encoder_.Profile().c_str());
}

void Session::GetStats(as_stats* out) {
    std::lock_guard<std::mutex> lock(mutex_);
    std::memset(out, 0, sizeof(*out));
    out->capture_fps = capture_fps_;
    out->encode_fps = encode_fps_;
    out->capture_ms = capture_ms_;
    out->encode_ms = encode_ms_;
    out->queue_ms = queue_ms_;
    out->qp = encoder_.LastQP();
    out->idle_skipped = idle_skipped_;
    out->dropped = dropped_;
    out->reacquires = reacquires_;
    out->width = converter_.DestWidth();
    out->height = converter_.DestHeight();
}

void Session::Close() {
    running_.store(false);
    frames_ready_.notify_all();
    if (thread_.joinable()) thread_.join();
    device_.Reset();
}

}  // namespace allshare

// ---------------------------------------------------------------------------
// C ABI
// ---------------------------------------------------------------------------

using allshare::Session;

extern "C" {

int32_t as_initialize(char* err, int32_t err_len) {
    std::lock_guard<std::mutex> lock(allshare::g_init_mutex);
    if (allshare::g_initialized.load()) return AS_OK;

    HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED | COINIT_DISABLE_OLE1DDE);
    if (FAILED(hr) && hr != RPC_E_CHANGED_MODE) {
        allshare::SetError(err, err_len, allshare::DescribeHResult("CoInitializeEx", hr));
        return AS_ERR_GENERIC;
    }
    hr = MFStartup(MF_VERSION, MFSTARTUP_NOSOCKET);
    if (FAILED(hr)) {
        allshare::SetError(err, err_len, allshare::DescribeHResult("MFStartup", hr));
        return AS_ERR_GENERIC;
    }
    allshare::g_initialized.store(true);
    return AS_OK;
}

void as_shutdown(void) {
    std::lock_guard<std::mutex> lock(allshare::g_init_mutex);
    if (!allshare::g_initialized.load()) return;
    MFShutdown();
    CoUninitialize();
    allshare::g_initialized.store(false);
}

int32_t as_enumerate_monitors(as_monitor* out, int32_t max_count) {
    if (!out || max_count <= 0) return 0;

    allshare::ComPtr<IDXGIFactory1> factory;
    if (FAILED(CreateDXGIFactory1(__uuidof(IDXGIFactory1),
                                  reinterpret_cast<void**>(factory.GetAddressOf())))) {
        return AS_ERR_GENERIC;
    }

    int32_t written = 0;
    for (UINT adapter_index = 0; written < max_count; ++adapter_index) {
        allshare::ComPtr<IDXGIAdapter1> adapter;
        if (factory->EnumAdapters1(adapter_index, adapter.GetAddressOf()) == DXGI_ERROR_NOT_FOUND) break;

        for (UINT output_index = 0; written < max_count; ++output_index) {
            allshare::ComPtr<IDXGIOutput> output;
            if (adapter->EnumOutputs(output_index, output.GetAddressOf()) == DXGI_ERROR_NOT_FOUND) break;

            DXGI_OUTPUT_DESC desc{};
            if (FAILED(output->GetDesc(&desc)) || !desc.AttachedToDesktop) continue;

            as_monitor& monitor = out[written];
            std::memset(&monitor, 0, sizeof(monitor));
            monitor.id = static_cast<int32_t>(output_index);
            monitor.x = desc.DesktopCoordinates.left;
            monitor.y = desc.DesktopCoordinates.top;
            monitor.width = desc.DesktopCoordinates.right - desc.DesktopCoordinates.left;
            monitor.height = desc.DesktopCoordinates.bottom - desc.DesktopCoordinates.top;
            monitor.primary = (desc.DesktopCoordinates.left == 0 && desc.DesktopCoordinates.top == 0) ? 1 : 0;
            allshare::QueryDisplayDetail(desc, &monitor.refresh_hz, &monitor.scale_percent);

            // A friendly name where Windows offers one, so the client's display
            // picker says "DELL U2720Q" rather than "Display 2".
            MONITORINFOEXW info{};
            info.cbSize = sizeof(info);
            if (GetMonitorInfoW(desc.Monitor, &info)) {
                WideCharToMultiByte(CP_UTF8, 0, info.szDevice, -1, monitor.name,
                                    static_cast<int>(sizeof(monitor.name)), nullptr, nullptr);
            }
            if (monitor.name[0] == '\0') {
                std::snprintf(monitor.name, sizeof(monitor.name), "Display %d", monitor.id + 1);
            }
            written++;
        }
    }
    return written;
}

int32_t as_query_capabilities(as_capability* out, int32_t max_count) {
    if (!out || max_count <= 0) return 0;

    int32_t written = 0;
    const struct { int32_t codec; const GUID* subtype; const char* profiles; } kCodecs[] = {
        // H.264 first: it is the only codec with hardware decode on effectively
        // every client this product targets, including Chromebooks.
        { AS_CODEC_H264, &MFVideoFormat_H264, "640c1f 4d001f 42e01f" },
        { AS_CODEC_H265, &MFVideoFormat_HEVC, "" },
    };

    for (const auto& entry : kCodecs) {
        if (written >= max_count) break;

        MFT_REGISTER_TYPE_INFO input{ MFMediaType_Video, MFVideoFormat_NV12 };
        MFT_REGISTER_TYPE_INFO output{ MFMediaType_Video, *entry.subtype };
        IMFActivate** activates = nullptr;
        UINT32 count = 0;
        bool hardware = true;

        HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                               MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                               &input, &output, &activates, &count);
        if (FAILED(hr) || count == 0) {
            if (activates) CoTaskMemFree(activates);
            activates = nullptr;
            count = 0;
            hardware = false;
            hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                           MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_ASYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER,
                           &input, &output, &activates, &count);
        }
        if (FAILED(hr) || count == 0) {
            if (activates) CoTaskMemFree(activates);
            continue;
        }

        as_capability& capability = out[written];
        std::memset(&capability, 0, sizeof(capability));
        capability.codec = entry.codec;
        capability.hardware = hardware ? 1 : 0;
        capability.max_width = 4096;
        capability.max_height = 4096;
        capability.max_fps = 240;
        std::snprintf(capability.profiles, sizeof(capability.profiles), "%s", entry.profiles);

        wchar_t* friendly = nullptr;
        UINT32 friendly_len = 0;
        activates[0]->GetAllocatedString(MFT_FRIENDLY_NAME_Attribute, &friendly, &friendly_len);
        if (friendly) {
            WideCharToMultiByte(CP_UTF8, 0, friendly, -1, capability.encoder,
                                static_cast<int>(sizeof(capability.encoder)), nullptr, nullptr);
            CoTaskMemFree(friendly);
        }
        if (capability.encoder[0] == '\0') {
            std::snprintf(capability.encoder, sizeof(capability.encoder), "%s encoder",
                          hardware ? "Hardware" : "Software");
        }

        for (UINT32 index = 0; index < count; ++index) activates[index]->Release();
        CoTaskMemFree(activates);
        written++;
    }
    return written;
}

as_capture* as_open(const as_open_options* options, char* err, int32_t err_len) {
    if (!options) {
        allshare::SetError(err, err_len, "no capture options were given");
        return nullptr;
    }
    if (as_initialize(err, err_len) != AS_OK) return nullptr;

    auto* session = new (std::nothrow) Session();
    if (!session) {
        allshare::SetError(err, err_len, "out of memory");
        return nullptr;
    }
    std::string error;
    if (!session->Open(*options, &error)) {
        allshare::SetError(err, err_len, error);
        delete session;
        return nullptr;
    }
    return reinterpret_cast<as_capture*>(session);
}

int32_t as_next_frame(as_capture* handle, int32_t timeout_ms, as_frame* out) {
    if (!handle || !out) return AS_ERR_CLOSED;
    return reinterpret_cast<Session*>(handle)->NextFrame(timeout_ms, out);
}

int32_t as_next_cursor(as_capture* handle, as_cursor* out) {
    if (!handle || !out) return 0;
    return reinterpret_cast<Session*>(handle)->NextCursor(out);
}

int32_t as_set_bitrate(as_capture* handle, int32_t bits_per_second) {
    if (!handle) return AS_ERR_CLOSED;
    reinterpret_cast<Session*>(handle)->SetBitrate(bits_per_second);
    return AS_OK;
}

int32_t as_set_framerate(as_capture* handle, int32_t fps) {
    if (!handle) return AS_ERR_CLOSED;
    reinterpret_cast<Session*>(handle)->SetFrameRate(fps);
    return AS_OK;
}

int32_t as_set_resolution(as_capture* handle, int32_t width, int32_t height) {
    if (!handle) return AS_ERR_CLOSED;
    return reinterpret_cast<Session*>(handle)->SetResolution(width, height);
}

int32_t as_set_monitor(as_capture* handle, int32_t monitor_id) {
    if (!handle) return AS_ERR_CLOSED;
    return reinterpret_cast<Session*>(handle)->SetMonitor(monitor_id);
}

int32_t as_set_preset(as_capture* handle, int32_t preset) {
    if (!handle) return AS_ERR_CLOSED;
    reinterpret_cast<Session*>(handle)->SetPreset(preset);
    return AS_OK;
}

void as_request_keyframe(as_capture* handle) {
    if (handle) reinterpret_cast<Session*>(handle)->RequestKeyframe();
}

void as_note_input(as_capture* handle, uint32_t sequence) {
    if (handle) reinterpret_cast<Session*>(handle)->NoteInput(sequence);
}

int32_t as_get_info(as_capture* handle, as_info* out) {
    if (!handle || !out) return AS_ERR_CLOSED;
    reinterpret_cast<Session*>(handle)->GetInfo(out);
    return AS_OK;
}

int32_t as_get_stats(as_capture* handle, as_stats* out) {
    if (!handle || !out) return AS_ERR_CLOSED;
    reinterpret_cast<Session*>(handle)->GetStats(out);
    return AS_OK;
}

void as_close(as_capture* handle) {
    if (!handle) return;
    auto* session = reinterpret_cast<Session*>(handle);
    session->Close();
    delete session;
}

}  // extern "C"
