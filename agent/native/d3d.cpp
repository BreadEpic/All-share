// Direct3D device setup and GPU colour conversion for ALL SHARE.
#include "internal.h"

#include <cstdio>
#include <cstring>

namespace allshare {

std::string DescribeHResult(const char* what, HRESULT hr) {
    char buffer[256];
    // FormatMessage gives a human sentence for most Windows errors; the raw
    // code is kept alongside it because support logs need something searchable.
    char* text = nullptr;
    DWORD length = FormatMessageA(
        FORMAT_MESSAGE_ALLOCATE_BUFFER | FORMAT_MESSAGE_FROM_SYSTEM | FORMAT_MESSAGE_IGNORE_INSERTS,
        nullptr, static_cast<DWORD>(hr), MAKELANGID(LANG_NEUTRAL, SUBLANG_DEFAULT),
        reinterpret_cast<char*>(&text), 0, nullptr);

    std::string detail;
    if (length && text) {
        detail = text;
        while (!detail.empty() && (detail.back() == '\n' || detail.back() == '\r' || detail.back() == ' ')) {
            detail.pop_back();
        }
    }
    if (text) LocalFree(text);

    std::snprintf(buffer, sizeof(buffer), "%s failed (0x%08lX)", what, static_cast<unsigned long>(hr));
    std::string out(buffer);
    if (!detail.empty()) out += ": " + detail;
    return out;
}

void SetError(char* buffer, int32_t length, const std::string& message) {
    if (!buffer || length <= 0) return;
    const size_t copy = message.size() < static_cast<size_t>(length - 1)
        ? message.size() : static_cast<size_t>(length - 1);
    std::memcpy(buffer, message.data(), copy);
    buffer[copy] = '\0';
}

int64_t UnixMicros() {
    FILETIME ft{};
    GetSystemTimePreciseAsFileTime(&ft);
    ULARGE_INTEGER value{};
    value.LowPart = ft.dwLowDateTime;
    value.HighPart = ft.dwHighDateTime;
    // FILETIME counts 100 ns intervals since 1601; shift to the Unix epoch.
    const uint64_t kEpochDelta = 116444736000000000ULL;
    return static_cast<int64_t>((value.QuadPart - kEpochDelta) / 10);
}

int64_t MonotonicMicros() {
    static LARGE_INTEGER frequency = [] {
        LARGE_INTEGER f{};
        QueryPerformanceFrequency(&f);
        return f;
    }();
    LARGE_INTEGER now{};
    QueryPerformanceCounter(&now);
    if (frequency.QuadPart == 0) return 0;
    return static_cast<int64_t>((now.QuadPart * 1000000LL) / frequency.QuadPart);
}

int32_t RoundUpTo(int32_t value, int32_t multiple) {
    if (multiple <= 1) return value;
    return ((value + multiple - 1) / multiple) * multiple;
}

// ---------------------------------------------------------------------------
// Device
// ---------------------------------------------------------------------------

bool Device::Create(std::string* error) {
    ComPtr<IDXGIFactory1> factory;
    HRESULT hr = CreateDXGIFactory1(__uuidof(IDXGIFactory1), reinterpret_cast<void**>(factory.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("CreateDXGIFactory1", hr);
        return false;
    }

    // Pick the adapter that actually drives a display. On a laptop with switchable
    // graphics the discrete GPU often has no outputs attached, and Desktop
    // Duplication only works on an adapter that owns the output being captured.
    ComPtr<IDXGIAdapter1> chosen;
    for (UINT index = 0;; ++index) {
        ComPtr<IDXGIAdapter1> candidate;
        if (factory->EnumAdapters1(index, candidate.GetAddressOf()) == DXGI_ERROR_NOT_FOUND) break;

        DXGI_ADAPTER_DESC1 desc{};
        candidate->GetDesc1(&desc);
        if (desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) continue;

        ComPtr<IDXGIOutput> output;
        if (SUCCEEDED(candidate->EnumOutputs(0, output.GetAddressOf()))) {
            chosen = candidate;
            adapter_name_.assign(desc.Description);
            break;
        }
        if (!chosen) chosen = candidate;
    }
    if (!chosen) {
        *error = "no usable graphics adapter was found";
        return false;
    }
    adapter_ = chosen;

    const D3D_FEATURE_LEVEL levels[] = {
        D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_1,
    };
    D3D_FEATURE_LEVEL achieved{};
    // BGRA_SUPPORT is needed for the format Desktop Duplication hands back;
    // VIDEO_SUPPORT is needed for the video processor and for the encoder MFT to
    // accept our textures.
    UINT flags = D3D11_CREATE_DEVICE_BGRA_SUPPORT | D3D11_CREATE_DEVICE_VIDEO_SUPPORT;
    hr = D3D11CreateDevice(adapter_.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr, flags,
                           levels, ARRAYSIZE(levels), D3D11_SDK_VERSION,
                           device_.GetAddressOf(), &achieved, context_.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("D3D11CreateDevice", hr);
        return false;
    }

    // The device is used from the capture thread and, inside the encoder MFT,
    // from a driver thread. Without this the driver is entitled to assume
    // single-threaded access and will corrupt state under load.
    ComPtr<ID3D10Multithread> multithread;
    if (SUCCEEDED(device_.As(&multithread))) {
        multithread->SetMultithreadProtected(TRUE);
    }

    hr = MFCreateDXGIDeviceManager(&manager_token_, manager_.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("MFCreateDXGIDeviceManager", hr);
        return false;
    }
    hr = manager_->ResetDevice(device_.Get(), manager_token_);
    if (FAILED(hr)) {
        *error = DescribeHResult("IMFDXGIDeviceManager::ResetDevice", hr);
        return false;
    }
    return true;
}

void Device::Reset() {
    manager_.Reset();
    context_.Reset();
    device_.Reset();
    adapter_.Reset();
}

// ---------------------------------------------------------------------------
// Converter
// ---------------------------------------------------------------------------

bool Converter::Create(Device* device, int32_t src_width, int32_t src_height,
                       int32_t dst_width, int32_t dst_height, std::string* error) {
    Reset();
    device_ = device;
    src_width_ = src_width;
    src_height_ = src_height;
    // Encoders work in macroblocks; an odd size costs a crop that shows as a
    // soft edge along one side of the picture.
    dst_width_ = RoundUpTo(dst_width, 2);
    dst_height_ = RoundUpTo(dst_height, 2);

    HRESULT hr = device->Get()->QueryInterface(__uuidof(ID3D11VideoDevice),
                                               reinterpret_cast<void**>(video_device_.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("ID3D11VideoDevice", hr);
        return false;
    }
    hr = device->Context()->QueryInterface(__uuidof(ID3D11VideoContext),
                                           reinterpret_cast<void**>(video_context_.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("ID3D11VideoContext", hr);
        return false;
    }

    D3D11_VIDEO_PROCESSOR_CONTENT_DESC desc{};
    desc.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    desc.InputWidth = static_cast<UINT>(src_width_);
    desc.InputHeight = static_cast<UINT>(src_height_);
    desc.OutputWidth = static_cast<UINT>(dst_width_);
    desc.OutputHeight = static_cast<UINT>(dst_height_);
    // Screen content, not film: tell the driver to favour throughput over
    // motion-compensated quality tricks that add latency.
    desc.Usage = D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;

    hr = video_device_->CreateVideoProcessorEnumerator(&desc, enumerator_.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("CreateVideoProcessorEnumerator", hr);
        return false;
    }
    hr = video_device_->CreateVideoProcessor(enumerator_.Get(), 0, processor_.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("CreateVideoProcessor", hr);
        return false;
    }

    D3D11_TEXTURE2D_DESC texture{};
    texture.Width = static_cast<UINT>(dst_width_);
    texture.Height = static_cast<UINT>(dst_height_);
    texture.MipLevels = 1;
    texture.ArraySize = 1;
    texture.Format = DXGI_FORMAT_NV12;
    texture.SampleDesc.Count = 1;
    texture.Usage = D3D11_USAGE_DEFAULT;
    texture.BindFlags = D3D11_BIND_RENDER_TARGET;
    // Shared so the encoder MFT can take it without a copy.
    texture.MiscFlags = D3D11_RESOURCE_MISC_SHARED;
    hr = device->Get()->CreateTexture2D(&texture, nullptr, output_.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("CreateTexture2D(NV12)", hr);
        return false;
    }

    D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC view{};
    view.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
    hr = video_device_->CreateVideoProcessorOutputView(output_.Get(), enumerator_.Get(), &view,
                                                       output_view_.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("CreateVideoProcessorOutputView", hr);
        return false;
    }

    // Full-range BT.709 in and out. Getting this wrong is the classic cause of
    // washed-out or crushed remote desktops, and it costs nothing to state.
    D3D11_VIDEO_PROCESSOR_COLOR_SPACE colour{};
    colour.Usage = 0;              // playback
    colour.RGB_Range = 0;          // full range
    colour.YCbCr_Matrix = 1;       // BT.709
    colour.YCbCr_xvYCC = 0;
    colour.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_0_255;
    video_context_->VideoProcessorSetStreamColorSpace(processor_.Get(), 0, &colour);
    video_context_->VideoProcessorSetOutputColorSpace(processor_.Get(), &colour);

    // No frame-rate conversion: one input frame must produce exactly one output
    // frame, or the encoder's timing and our latency accounting both drift.
    video_context_->VideoProcessorSetStreamFrameFormat(processor_.Get(), 0,
                                                       D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
    video_context_->VideoProcessorSetStreamAutoProcessingMode(processor_.Get(), 0, FALSE);
    return true;
}

void Converter::Reset() {
    output_view_.Reset();
    output_.Reset();
    processor_.Reset();
    enumerator_.Reset();
    video_context_.Reset();
    video_device_.Reset();
    src_width_ = src_height_ = dst_width_ = dst_height_ = 0;
}

ID3D11Texture2D* Converter::Convert(ID3D11Texture2D* src, std::string* error) {
    if (!processor_ || !src) {
        *error = "the video converter is not ready";
        return nullptr;
    }

    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC desc{};
    desc.FourCC = 0;
    desc.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
    desc.Texture2D.MipSlice = 0;
    desc.Texture2D.ArraySlice = 0;

    ComPtr<ID3D11VideoProcessorInputView> input_view;
    HRESULT hr = video_device_->CreateVideoProcessorInputView(src, enumerator_.Get(), &desc,
                                                              input_view.GetAddressOf());
    if (FAILED(hr)) {
        *error = DescribeHResult("CreateVideoProcessorInputView", hr);
        return nullptr;
    }

    D3D11_VIDEO_PROCESSOR_STREAM stream{};
    stream.Enable = TRUE;
    stream.OutputIndex = 0;
    stream.InputFrameOrField = 0;
    stream.pInputSurface = input_view.Get();

    hr = video_context_->VideoProcessorBlt(processor_.Get(), output_view_.Get(), 0, 1, &stream);
    if (FAILED(hr)) {
        *error = DescribeHResult("VideoProcessorBlt", hr);
        return nullptr;
    }
    return output_.Get();
}

}  // namespace allshare
