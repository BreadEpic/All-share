// System audio capture and Opus encoding for ALL SHARE.
//
// Audio is captured with WASAPI loopback on the default playback device, which
// is what "hear what the PC is playing" actually means on Windows: it taps the
// mix the audio engine is about to send to the speakers, so it includes every
// application without any of them cooperating.
//
// Opus is the only sensible codec here. It is the one audio codec every browser
// decodes over WebRTC, it is designed for interactive latency rather than
// broadcast, and at 128 kbit/s stereo it is transparent for the desktop sounds
// and music this carries. G.722 would avoid the dependency and sounds like a
// telephone; raw PCM would cost 1.5 Mbit/s to send something Opus does in a
// twelfth of that.
#include "internal.h"

#include <cstring>
#include <algorithm>

#include <mmdeviceapi.h>
#include <audioclient.h>
#include <functiondiscoverykeys_devpkey.h>

#ifdef ALLSHARE_WITH_OPUS
#include <opus.h>
#endif

namespace allshare {

namespace {

// Audio subformat identifiers, spelled out because not every Windows toolchain
// ships definitions for them. These values are fixed by Windows: they are the
// WAVE format tags promoted into GUID space by WAVEFORMATEXTENSIBLE.
const GUID kSubtypeIEEEFloat = {
    0x00000003, 0x0000, 0x0010, { 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71 }
};
const GUID kSubtypePCM = {
    0x00000001, 0x0000, 0x0010, { 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71 }
};

// Opus works natively at 48 kHz, which is also the mix rate on most Windows
// systems, so the common case needs no conversion at all.
constexpr int kOpusRate = 48000;
constexpr int kOpusChannels = 2;

// A 20 ms frame is the usual interactive trade: 10 ms halves the algorithmic
// delay but adds noticeable per-packet overhead, and 40 ms is audible as lag
// against a video stream that is already inside 30 ms.
constexpr int kFrameMillis = 20;
constexpr int kSamplesPerFrame = kOpusRate * kFrameMillis / 1000;

// Catmull-Rom interpolation, used only when the Windows mix rate is not 48 kHz
// (44.1 kHz being the common case).
//
// It is not a windowed-sinc resampler, and for a 44.1-to-48 conversion of
// desktop audio the difference is not audible; a full polyphase filter would be
// more code in a path that most machines never take.
float Interpolate(float a, float b, float c, float d, float t) {
    const float t2 = t * t;
    const float t3 = t2 * t;
    return 0.5f * ((2 * b) + (-a + c) * t +
                   (2 * a - 5 * b + 4 * c - d) * t2 +
                   (-a + 3 * b - 3 * c + d) * t3);
}

// Unused for now, but kept next to its pair so the two subformats are defined
// and documented together rather than one appearing on its own later.
[[maybe_unused]] const GUID& PcmSubtype() { return kSubtypePCM; }

}  // namespace

// AudioCapture owns the WASAPI client, its thread, and the Opus encoder.
class AudioCapture {
public:
    bool Start(int32_t bitrate, std::string* error);
    void Stop();

    // Takes the next encoded packet, or returns false if none is ready.
    bool Next(std::vector<uint8_t>* data, int32_t* duration_us, int64_t* captured_at_us);

    bool Running() const { return running_.load(); }

private:
    void Run();
    bool Initialise(std::string* error);
    void PushSamples(const float* interleaved, int32_t frames, int32_t source_channels);
    void EncodeReady(int64_t captured_at_us);

    ComPtr<IMMDeviceEnumerator> enumerator_;
    ComPtr<IMMDevice> device_;
    ComPtr<IAudioClient> client_;
    ComPtr<IAudioCaptureClient> capture_;
    WAVEFORMATEX* mix_format_ = nullptr;

#ifdef ALLSHARE_WITH_OPUS
    OpusEncoder* encoder_ = nullptr;
#endif

    std::thread thread_;
    std::atomic<bool> running_{false};
    int32_t bitrate_ = 128000;

    // Interleaved stereo at 48 kHz, awaiting a whole Opus frame.
    std::vector<float> pending_;
    // Resampler history, kept across buffers so interpolation does not glitch
    // at buffer boundaries.
    float history_[4][2] = {};
    double resample_position_ = 0;

    std::mutex mutex_;
    struct Packet {
        std::vector<uint8_t> data;
        int32_t duration_us;
        int64_t captured_at_us;
    };
    std::deque<Packet> packets_;
};

bool AudioCapture::Start(int32_t bitrate, std::string* error) {
#ifndef ALLSHARE_WITH_OPUS
    *error = "this build of ALL SHARE was made without audio support";
    return false;
#else
    if (bitrate > 0) bitrate_ = std::clamp(bitrate, 24000, 320000);
    if (!Initialise(error)) return false;
    running_.store(true);
    thread_ = std::thread([this] { Run(); });
    return true;
#endif
}

bool AudioCapture::Initialise(std::string* error) {
#ifdef ALLSHARE_WITH_OPUS
    HRESULT hr = CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL,
                                  __uuidof(IMMDeviceEnumerator),
                                  reinterpret_cast<void**>(enumerator_.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("open the audio device list", hr);
        return false;
    }
    // eRender, not eCapture: loopback taps the playback device, which is what
    // carries the sound the user is hearing.
    hr = enumerator_->GetDefaultAudioEndpoint(eRender, eConsole, device_.GetAddressOf());
    if (FAILED(hr)) {
        *error = "this PC has no active playback device to capture sound from";
        return false;
    }
    hr = device_->Activate(__uuidof(IAudioClient), CLSCTX_ALL, nullptr,
                           reinterpret_cast<void**>(client_.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("open the audio device", hr);
        return false;
    }
    hr = client_->GetMixFormat(&mix_format_);
    if (FAILED(hr) || !mix_format_) {
        *error = DescribeHResult("read the audio format", hr);
        return false;
    }

    // Loopback capture cannot use event-driven notification, so the buffer is
    // polled. A 40 ms buffer is deep enough to survive a scheduling hiccup and
    // shallow enough not to add meaningful latency.
    const REFERENCE_TIME duration = 40 * 10000;
    hr = client_->Initialize(AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK,
                             duration, 0, mix_format_, nullptr);
    if (FAILED(hr)) {
        *error = DescribeHResult("start audio capture", hr);
        return false;
    }
    hr = client_->GetService(__uuidof(IAudioCaptureClient),
                             reinterpret_cast<void**>(capture_.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("start audio capture", hr);
        return false;
    }

    int opus_error = 0;
    // OPUS_APPLICATION_AUDIO rather than VOIP: this carries music and system
    // sounds, not speech, and the speech-tuned mode audibly damages both.
    encoder_ = opus_encoder_create(kOpusRate, kOpusChannels, OPUS_APPLICATION_AUDIO, &opus_error);
    if (!encoder_ || opus_error != OPUS_OK) {
        *error = "could not start the audio encoder";
        return false;
    }
    opus_encoder_ctl(encoder_, OPUS_SET_BITRATE(bitrate_));
    // In-band forward error correction plus an expected loss figure lets Opus
    // carry enough redundancy to survive the packet loss a real network has,
    // which matters more for audio than for video: a dropped video frame is a
    // flicker, a dropped audio packet is an audible click.
    opus_encoder_ctl(encoder_, OPUS_SET_INBAND_FEC(1));
    opus_encoder_ctl(encoder_, OPUS_SET_PACKET_LOSS_PERC(5));
    opus_encoder_ctl(encoder_, OPUS_SET_SIGNAL(OPUS_SIGNAL_MUSIC));
    // Complexity 5 is the knee of the curve: near-maximum quality for roughly
    // half the CPU of complexity 10, which matters when a game is already using
    // the machine.
    opus_encoder_ctl(encoder_, OPUS_SET_COMPLEXITY(5));

    hr = client_->Start();
    if (FAILED(hr)) {
        *error = DescribeHResult("start audio capture", hr);
        return false;
    }
    return true;
#else
    *error = "audio support was not built in";
    return false;
#endif
}

void AudioCapture::Run() {
#ifdef ALLSHARE_WITH_OPUS
    CoInitializeEx(nullptr, COINIT_MULTITHREADED | COINIT_DISABLE_OLE1DDE);

    const int32_t source_channels = mix_format_ ? mix_format_->nChannels : 2;
    const bool is_float = mix_format_ &&
        (mix_format_->wFormatTag == WAVE_FORMAT_IEEE_FLOAT ||
         (mix_format_->wFormatTag == WAVE_FORMAT_EXTENSIBLE &&
          IsEqualGUID(reinterpret_cast<WAVEFORMATEXTENSIBLE*>(mix_format_)->SubFormat,
                      kSubtypeIEEEFloat)));
    const int32_t bytes_per_sample = mix_format_ ? mix_format_->wBitsPerSample / 8 : 4;

    std::vector<float> scratch;
    while (running_.load()) {
        UINT32 available = 0;
        if (FAILED(capture_->GetNextPacketSize(&available)) || available == 0) {
            std::this_thread::sleep_for(std::chrono::milliseconds(5));
            continue;
        }

        while (available > 0 && running_.load()) {
            BYTE* data = nullptr;
            UINT32 frames = 0;
            DWORD flags = 0;
            if (FAILED(capture_->GetBuffer(&data, &frames, &flags, nullptr, nullptr))) break;

            const int64_t captured_at = UnixMicros();
            scratch.assign(static_cast<size_t>(frames) * source_channels, 0.0f);

            // A silent buffer still has to advance the timeline, or audio and
            // video drift apart every time nothing is playing.
            if (!(flags & AUDCLNT_BUFFERFLAGS_SILENT) && data) {
                if (is_float && bytes_per_sample == 4) {
                    std::memcpy(scratch.data(), data, scratch.size() * sizeof(float));
                } else if (bytes_per_sample == 2) {
                    const int16_t* pcm = reinterpret_cast<const int16_t*>(data);
                    for (size_t i = 0; i < scratch.size(); ++i) {
                        scratch[i] = static_cast<float>(pcm[i]) / 32768.0f;
                    }
                }
            }

            PushSamples(scratch.data(), static_cast<int32_t>(frames), source_channels);
            EncodeReady(captured_at);

            capture_->ReleaseBuffer(frames);
            if (FAILED(capture_->GetNextPacketSize(&available))) break;
        }
    }

    client_->Stop();
    CoUninitialize();
#endif
}

// PushSamples downmixes to stereo and resamples to 48 kHz.
void AudioCapture::PushSamples(const float* interleaved, int32_t frames, int32_t source_channels) {
    if (frames <= 0 || source_channels <= 0) return;
    const double source_rate = mix_format_ ? mix_format_->nSamplesPerSec : kOpusRate;
    const double ratio = source_rate / static_cast<double>(kOpusRate);

    for (int32_t frame = 0; frame < frames; ++frame) {
        // Anything above stereo is folded down by averaging the extra channels
        // into both sides. A proper 5.1 downmix matrix would be better, and is
        // not worth the complexity for a remote desktop.
        float left = interleaved[static_cast<size_t>(frame) * source_channels];
        float right = source_channels > 1
            ? interleaved[static_cast<size_t>(frame) * source_channels + 1]
            : left;
        for (int32_t channel = 2; channel < source_channels; ++channel) {
            const float extra = interleaved[static_cast<size_t>(frame) * source_channels + channel];
            left += extra * 0.5f;
            right += extra * 0.5f;
        }

        history_[0][0] = history_[1][0]; history_[0][1] = history_[1][1];
        history_[1][0] = history_[2][0]; history_[1][1] = history_[2][1];
        history_[2][0] = history_[3][0]; history_[2][1] = history_[3][1];
        history_[3][0] = std::clamp(left, -1.0f, 1.0f);
        history_[3][1] = std::clamp(right, -1.0f, 1.0f);

        // The common case: the mix is already 48 kHz, so this is a copy.
        if (ratio > 0.9999 && ratio < 1.0001) {
            pending_.push_back(history_[3][0]);
            pending_.push_back(history_[3][1]);
            continue;
        }

        resample_position_ += 1.0 / ratio;
        while (resample_position_ >= 1.0) {
            resample_position_ -= 1.0;
            const float t = static_cast<float>(1.0 - resample_position_);
            pending_.push_back(Interpolate(history_[0][0], history_[1][0],
                                           history_[2][0], history_[3][0], t));
            pending_.push_back(Interpolate(history_[0][1], history_[1][1],
                                           history_[2][1], history_[3][1], t));
        }
    }
}

void AudioCapture::EncodeReady(int64_t captured_at_us) {
#ifdef ALLSHARE_WITH_OPUS
    const size_t needed = static_cast<size_t>(kSamplesPerFrame) * kOpusChannels;
    uint8_t buffer[4000];

    while (pending_.size() >= needed) {
        const int encoded = opus_encode_float(encoder_, pending_.data(), kSamplesPerFrame,
                                              buffer, static_cast<opus_int32>(sizeof(buffer)));
        pending_.erase(pending_.begin(), pending_.begin() + needed);
        if (encoded <= 0) continue;

        std::lock_guard<std::mutex> lock(mutex_);
        // Audio that has queued up is audio that will arrive late and out of
        // sync. Half a second is already far past useful.
        while (packets_.size() >= 25) packets_.pop_front();
        packets_.push_back(Packet{
            std::vector<uint8_t>(buffer, buffer + encoded),
            kFrameMillis * 1000,
            captured_at_us,
        });
    }
#endif
}

bool AudioCapture::Next(std::vector<uint8_t>* data, int32_t* duration_us, int64_t* captured_at_us) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (packets_.empty()) return false;
    *data = std::move(packets_.front().data);
    *duration_us = packets_.front().duration_us;
    *captured_at_us = packets_.front().captured_at_us;
    packets_.pop_front();
    return true;
}

void AudioCapture::Stop() {
    running_.store(false);
    if (thread_.joinable()) thread_.join();
#ifdef ALLSHARE_WITH_OPUS
    if (encoder_) {
        opus_encoder_destroy(encoder_);
        encoder_ = nullptr;
    }
#endif
    if (mix_format_) {
        CoTaskMemFree(mix_format_);
        mix_format_ = nullptr;
    }
    capture_.Reset();
    client_.Reset();
    device_.Reset();
    enumerator_.Reset();
}

// ---------------------------------------------------------------------------
// C ABI
// ---------------------------------------------------------------------------

extern "C" {

int32_t as_audio_available(void) {
#ifdef ALLSHARE_WITH_OPUS
    return 1;
#else
    return 0;
#endif
}

as_audio* as_audio_open(int32_t bitrate, char* err, int32_t err_len) {
    auto* capture = new (std::nothrow) AudioCapture();
    if (!capture) {
        SetError(err, err_len, "out of memory");
        return nullptr;
    }
    std::string error;
    if (!capture->Start(bitrate, &error)) {
        SetError(err, err_len, error);
        delete capture;
        return nullptr;
    }
    return reinterpret_cast<as_audio*>(capture);
}

int32_t as_audio_next(as_audio* handle, as_audio_frame* out) {
    if (!handle || !out) return 0;
    auto* capture = reinterpret_cast<AudioCapture*>(handle);

    // The returned buffer stays valid until the next call on this handle, which
    // matches how the video path works and keeps the Go side simple.
    static thread_local std::vector<uint8_t> held;
    int32_t duration_us = 0;
    int64_t captured_at = 0;
    if (!capture->Next(&held, &duration_us, &captured_at)) return 0;

    out->data = held.data();
    out->size = static_cast<int32_t>(held.size());
    out->duration_us = duration_us;
    out->capture_time_us = captured_at;
    return 1;
}

void as_audio_close(as_audio* handle) {
    if (!handle) return;
    auto* capture = reinterpret_cast<AudioCapture*>(handle);
    capture->Stop();
    delete capture;
}

}  // extern "C"

}  // namespace allshare
