// Shared C++ types for the ALL SHARE native capture layer.
#pragma once

#define COBJMACROS
#define INITGUID
#define WIN32_LEAN_AND_MEAN
#define NOMINMAX

#include <windows.h>
#include <d3d11.h>
#include <d3d11_1.h>
#include <dxgi1_2.h>
#include <dxgi1_6.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mferror.h>
#include <codecapi.h>
#include <wrl/client.h>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <deque>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "allshare_capture.h"

namespace allshare {

using Microsoft::WRL::ComPtr;

// Formats a Windows error into something a support log can act on.
std::string DescribeHResult(const char* what, HRESULT hr);

// Writes a message into a caller-supplied buffer, always null terminated.
void SetError(char* buffer, int32_t length, const std::string& message);

// Microseconds since the Unix epoch, from the same clock the rest of the agent
// uses, so a capture timestamp is directly comparable with a client's.
int64_t UnixMicros();

// A monotonic microsecond counter for measuring durations.
int64_t MonotonicMicros();

// Rounds up to a multiple, used to keep encoder dimensions on macroblock
// boundaries. Hardware encoders pad anyway, and an unpadded size costs a crop
// that shows as a soft edge.
int32_t RoundUpTo(int32_t value, int32_t multiple);

// ---------------------------------------------------------------------------
// Direct3D
// ---------------------------------------------------------------------------

// Device owns the D3D11 device shared by capture, scaling and encoding.
//
// One device for all three is what makes the pipeline zero-copy: a texture
// produced by Desktop Duplication can be fed to the video processor and then to
// the encoder without ever crossing the PCIe bus.
class Device {
public:
    bool Create(std::string* error);
    void Reset();

    ID3D11Device* Get() const { return device_.Get(); }
    ID3D11DeviceContext* Context() const { return context_.Get(); }
    IDXGIAdapter1* Adapter() const { return adapter_.Get(); }
    IMFDXGIDeviceManager* Manager() const { return manager_.Get(); }
    const std::wstring& AdapterName() const { return adapter_name_; }

private:
    ComPtr<ID3D11Device> device_;
    ComPtr<ID3D11DeviceContext> context_;
    ComPtr<IDXGIAdapter1> adapter_;
    ComPtr<IMFDXGIDeviceManager> manager_;
    UINT manager_token_ = 0;
    std::wstring adapter_name_;
};

// ---------------------------------------------------------------------------
// Colour conversion and scaling
// ---------------------------------------------------------------------------

// Converter turns the BGRA texture Desktop Duplication produces into the NV12
// the hardware encoder wants, scaling in the same pass.
//
// Doing both on the GPU with the video processor matters twice over: it keeps
// the frame in VRAM, and it uses the fixed-function scaler, which is both free
// and better than a naive shader for the downscales the quality ladder asks for.
class Converter {
public:
    bool Create(Device* device, int32_t src_width, int32_t src_height,
                int32_t dst_width, int32_t dst_height, std::string* error);
    void Reset();

    // Converts src into the internal NV12 texture and returns it.
    ID3D11Texture2D* Convert(ID3D11Texture2D* src, std::string* error);

    int32_t DestWidth() const { return dst_width_; }
    int32_t DestHeight() const { return dst_height_; }
    bool Matches(int32_t src_w, int32_t src_h, int32_t dst_w, int32_t dst_h) const {
        return src_width_ == src_w && src_height_ == src_h &&
               dst_width_ == dst_w && dst_height_ == dst_h;
    }

private:
    Device* device_ = nullptr;
    ComPtr<ID3D11VideoDevice> video_device_;
    ComPtr<ID3D11VideoContext> video_context_;
    ComPtr<ID3D11VideoProcessor> processor_;
    ComPtr<ID3D11VideoProcessorEnumerator> enumerator_;
    ComPtr<ID3D11Texture2D> output_;
    ComPtr<ID3D11VideoProcessorOutputView> output_view_;
    int32_t src_width_ = 0, src_height_ = 0;
    int32_t dst_width_ = 0, dst_height_ = 0;
};

// ---------------------------------------------------------------------------
// Desktop capture
// ---------------------------------------------------------------------------

struct CapturedFrame {
    ID3D11Texture2D* texture = nullptr;  // borrowed; valid until Release()
    bool desktop_changed = false;
    bool cursor_changed = false;
    int64_t captured_at_us = 0;
};

// Duplicator wraps DXGI Desktop Duplication.
//
// Desktop Duplication is used rather than Windows Graphics Capture because it
// reports whether the desktop image actually changed. That single fact is the
// largest saving in the whole product: on a desktop most frames are identical,
// and skipping them costs nothing while letting every available bit go to the
// pixels that did change — which is why still text converges to sharp instead
// of being re-encoded forever.
class Duplicator {
public:
    bool Create(Device* device, int32_t monitor_id, std::string* error);
    void Reset();

    // Waits for the next frame. Returns AS_OK, AS_TIMEOUT, or an error.
    // AS_ERR_LOST means the desktop changed underneath us — a resolution
    // change, a UAC prompt, a lock, a user switch — and the caller should
    // recreate the duplicator rather than give up.
    int32_t Acquire(int32_t timeout_ms, CapturedFrame* out, std::string* error);
    void Release();

    int32_t Width() const { return width_; }
    int32_t Height() const { return height_; }
    int32_t MonitorId() const { return monitor_id_; }

    // Cursor state, updated as part of Acquire.
    bool TakeCursorShape(std::vector<uint8_t>* bgra, int32_t* width, int32_t* height,
                         int32_t* hot_x, int32_t* hot_y, uint32_t* shape_id);
    void CursorPosition(int32_t* x, int32_t* y, bool* visible) const;

private:
    bool UpdateCursor(const DXGI_OUTDUPL_FRAME_INFO& info, std::string* error);
    bool DecodeShape(const DXGI_OUTDUPL_POINTER_SHAPE_INFO& info,
                     const std::vector<uint8_t>& raw);

    Device* device_ = nullptr;
    ComPtr<IDXGIOutputDuplication> duplication_;
    ComPtr<IDXGIOutput1> output_;
    ComPtr<ID3D11Texture2D> acquired_;
    bool holding_ = false;

    int32_t monitor_id_ = 0;
    int32_t width_ = 0;
    int32_t height_ = 0;

    std::vector<uint8_t> shape_raw_;
    std::vector<uint8_t> shape_bgra_;
    int32_t shape_width_ = 0, shape_height_ = 0;
    int32_t shape_hot_x_ = 0, shape_hot_y_ = 0;
    uint32_t shape_id_ = 0;
    bool shape_pending_ = false;
    int32_t cursor_x_ = 0, cursor_y_ = 0;
    bool cursor_visible_ = false;
};

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

struct EncodedPacket {
    std::vector<uint8_t> data;
    bool keyframe = false;
    int64_t capture_time_us = 0;
    int32_t encode_us = 0;
    int32_t width = 0;
    int32_t height = 0;
    uint32_t input_sequence = 0;
};

// Encoder drives a hardware Media Foundation video encoder.
class Encoder {
public:
    bool Create(Device* device, int32_t codec, int32_t width, int32_t height,
                int32_t fps, int32_t bitrate, int32_t preset, std::string* error);
    void Reset();

    // Submits an NV12 texture. Encoded output is appended to out.
    bool Encode(ID3D11Texture2D* nv12, int64_t capture_time_us, bool force_keyframe,
                std::vector<EncodedPacket>* out, std::string* error);
    // Drains any output the encoder is still holding.
    bool Drain(std::vector<EncodedPacket>* out, std::string* error);

    bool SetBitrate(int32_t bits_per_second);
    bool SetFrameRate(int32_t fps);

    const std::string& Name() const { return name_; }
    bool Hardware() const { return hardware_; }
    const std::string& Profile() const { return profile_; }
    int32_t Width() const { return width_; }
    int32_t Height() const { return height_; }
    float LastQP() const { return last_qp_; }

private:
    bool ConfigureCodecApi(int32_t bitrate, int32_t fps, int32_t preset, std::string* error);
    bool CollectOutput(std::vector<EncodedPacket>* out, std::string* error);
    void AppendAnnexB(const uint8_t* data, int32_t size, bool keyframe,
                      int64_t capture_time_us, int32_t encode_us,
                      std::vector<EncodedPacket>* out);

    Device* device_ = nullptr;
    ComPtr<IMFTransform> transform_;
    ComPtr<ICodecAPI> codec_api_;
    ComPtr<IMFMediaEventGenerator> events_;
    std::string name_;
    std::string profile_;
    bool hardware_ = false;
    bool async_ = false;
    int32_t codec_ = AS_CODEC_H264;
    int32_t width_ = 0, height_ = 0, fps_ = 60;
    int64_t frame_index_ = 0;
    int64_t encode_started_us_ = 0;
    float last_qp_ = 0;
    std::vector<uint8_t> sequence_header_;
    bool sent_sequence_header_ = false;

    // Frames submitted but not yet returned, so an output sample can be matched
    // back to the moment its pixels were captured.
    struct PendingFrame {
        int64_t timestamp_100ns;
        int64_t capture_time_us;
        int64_t submitted_us;
    };
    std::deque<PendingFrame> pending_;
};

}  // namespace allshare
