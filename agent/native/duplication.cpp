// DXGI Desktop Duplication capture for ALL SHARE.
#include "internal.h"

#include <cstring>

namespace allshare {
namespace {

// Finds the IDXGIOutput matching a monitor index across all adapters.
bool FindOutput(IDXGIAdapter1* adapter, int32_t monitor_id,
                ComPtr<IDXGIOutput1>* out, DXGI_OUTPUT_DESC* desc_out) {
    for (UINT index = 0;; ++index) {
        ComPtr<IDXGIOutput> output;
        if (adapter->EnumOutputs(index, output.GetAddressOf()) == DXGI_ERROR_NOT_FOUND) break;
        DXGI_OUTPUT_DESC desc{};
        if (FAILED(output->GetDesc(&desc))) continue;
        if (!desc.AttachedToDesktop) continue;
        if (static_cast<int32_t>(index) != monitor_id) continue;

        ComPtr<IDXGIOutput1> output1;
        if (FAILED(output.As(&output1))) return false;
        *out = output1;
        *desc_out = desc;
        return true;
    }
    return false;
}

}  // namespace

bool Duplicator::Create(Device* device, int32_t monitor_id, std::string* error) {
    Reset();
    device_ = device;
    monitor_id_ = monitor_id;

    DXGI_OUTPUT_DESC desc{};
    if (!FindOutput(device->Adapter(), monitor_id, &output_, &desc)) {
        // Fall back to the first attached output. A monitor can be unplugged
        // between the client choosing it and the session starting, and showing
        // the main display beats failing the connection.
        if (!FindOutput(device->Adapter(), 0, &output_, &desc)) {
            *error = "no display is attached to this graphics adapter";
            return false;
        }
        monitor_id_ = 0;
    }

    width_ = desc.DesktopCoordinates.right - desc.DesktopCoordinates.left;
    height_ = desc.DesktopCoordinates.bottom - desc.DesktopCoordinates.top;

    HRESULT hr = output_->DuplicateOutput(device->Get(), duplication_.GetAddressOf());
    if (FAILED(hr)) {
        if (hr == DXGI_ERROR_NOT_CURRENTLY_AVAILABLE) {
            // Windows allows a limited number of duplications per output.
            *error = "another program is already capturing this screen";
        } else if (hr == E_ACCESSDENIED) {
            // This is the session-0 case, and the reason the agent runs a
            // desktop helper rather than capturing from the service itself.
            *error = "this program is not allowed to capture the screen from where it is running";
        } else {
            *error = DescribeHResult("DuplicateOutput", hr);
        }
        return false;
    }
    return true;
}

void Duplicator::Reset() {
    Release();
    duplication_.Reset();
    output_.Reset();
    acquired_.Reset();
    width_ = height_ = 0;
    shape_pending_ = false;
    shape_bgra_.clear();
}

void Duplicator::Release() {
    if (holding_ && duplication_) {
        duplication_->ReleaseFrame();
    }
    holding_ = false;
    acquired_.Reset();
}

int32_t Duplicator::Acquire(int32_t timeout_ms, CapturedFrame* out, std::string* error) {
    if (!duplication_) {
        *error = "screen capture is not running";
        return AS_ERR_CLOSED;
    }
    Release();

    DXGI_OUTDUPL_FRAME_INFO info{};
    ComPtr<IDXGIResource> resource;
    HRESULT hr = duplication_->AcquireNextFrame(static_cast<UINT>(timeout_ms), &info,
                                                resource.GetAddressOf());
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
        return AS_TIMEOUT;
    }
    if (hr == DXGI_ERROR_ACCESS_LOST || hr == DXGI_ERROR_INVALID_CALL) {
        // The desktop changed underneath us: a resolution change, a UAC prompt,
        // a lock, a fast user switch, or a GPU mode change. Recoverable, and
        // routine — the caller recreates and carries on.
        *error = "the desktop changed and capture must restart";
        return AS_ERR_LOST;
    }
    if (FAILED(hr)) {
        *error = DescribeHResult("AcquireNextFrame", hr);
        return AS_ERR_GENERIC;
    }
    holding_ = true;

    if (!UpdateCursor(info, error)) {
        // A cursor failure must not stop the video; the client falls back to
        // drawing the pointer it already has.
        error->clear();
    }

    out->captured_at_us = UnixMicros();
    out->cursor_changed = info.LastMouseUpdateTime.QuadPart != 0;

    // LastPresentTime is zero when only the pointer moved. Skipping the encode
    // in that case is the single largest saving in the product: on a desktop
    // most frames are identical, and every bit not spent re-sending an
    // unchanged screen goes to the pixels that did change.
    out->desktop_changed = info.LastPresentTime.QuadPart != 0 && info.AccumulatedFrames > 0;
    if (!out->desktop_changed) {
        out->texture = nullptr;
        return AS_OK;
    }

    hr = resource->QueryInterface(__uuidof(ID3D11Texture2D),
                                  reinterpret_cast<void**>(acquired_.GetAddressOf()));
    if (FAILED(hr)) {
        *error = DescribeHResult("QueryInterface(ID3D11Texture2D)", hr);
        return AS_ERR_GENERIC;
    }
    out->texture = acquired_.Get();
    return AS_OK;
}

bool Duplicator::UpdateCursor(const DXGI_OUTDUPL_FRAME_INFO& info, std::string* error) {
    if (info.LastMouseUpdateTime.QuadPart != 0) {
        cursor_visible_ = info.PointerPosition.Visible != FALSE;
        cursor_x_ = info.PointerPosition.Position.x;
        cursor_y_ = info.PointerPosition.Position.y;
    }
    if (info.PointerShapeBufferSize == 0) return true;

    shape_raw_.resize(info.PointerShapeBufferSize);
    DXGI_OUTDUPL_POINTER_SHAPE_INFO shape{};
    UINT required = 0;
    HRESULT hr = duplication_->GetFramePointerShape(
        static_cast<UINT>(shape_raw_.size()), shape_raw_.data(), &required, &shape);
    if (FAILED(hr)) {
        *error = DescribeHResult("GetFramePointerShape", hr);
        return false;
    }
    return DecodeShape(shape, shape_raw_);
}

// DecodeShape converts the three pointer formats Windows uses into plain BGRA.
//
// Monochrome and masked-colour pointers carry an AND mask that means "invert
// whatever is underneath". There is nothing underneath here, because the
// pointer is composited on the client, so those pixels are rendered as opaque
// black — which is what Windows itself draws over light backgrounds and is far
// less distracting than a hole in the cursor.
bool Duplicator::DecodeShape(const DXGI_OUTDUPL_POINTER_SHAPE_INFO& info,
                             const std::vector<uint8_t>& raw) {
    const int32_t width = static_cast<int32_t>(info.Width);
    int32_t height = static_cast<int32_t>(info.Height);
    if (width <= 0 || height <= 0 || width > 1024 || height > 2048) return false;

    const uint32_t pitch = info.Pitch;
    shape_hot_x_ = static_cast<int32_t>(info.HotSpot.x);
    shape_hot_y_ = static_cast<int32_t>(info.HotSpot.y);

    if (info.Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME) {
        // A monochrome pointer stores the AND mask above the XOR mask, so the
        // buffer is twice the visible height.
        height /= 2;
        if (height <= 0) return false;
        shape_bgra_.assign(static_cast<size_t>(width) * height * 4, 0);
        for (int32_t y = 0; y < height; ++y) {
            for (int32_t x = 0; x < width; ++x) {
                const size_t byte = static_cast<size_t>(y) * pitch + (x / 8);
                const size_t xor_byte = static_cast<size_t>(y + height) * pitch + (x / 8);
                if (xor_byte >= raw.size()) continue;
                const uint8_t bit = static_cast<uint8_t>(0x80 >> (x % 8));
                const bool and_bit = (raw[byte] & bit) != 0;
                const bool xor_bit = (raw[xor_byte] & bit) != 0;

                uint8_t* pixel = &shape_bgra_[(static_cast<size_t>(y) * width + x) * 4];
                if (!and_bit && !xor_bit) {          // opaque black
                    pixel[0] = pixel[1] = pixel[2] = 0; pixel[3] = 255;
                } else if (!and_bit && xor_bit) {     // opaque white
                    pixel[0] = pixel[1] = pixel[2] = 255; pixel[3] = 255;
                } else if (and_bit && !xor_bit) {     // transparent
                    pixel[3] = 0;
                } else {                              // "invert" — drawn black
                    pixel[0] = pixel[1] = pixel[2] = 0; pixel[3] = 255;
                }
            }
        }
    } else if (info.Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR) {
        shape_bgra_.assign(static_cast<size_t>(width) * height * 4, 0);
        for (int32_t y = 0; y < height; ++y) {
            const size_t row = static_cast<size_t>(y) * pitch;
            if (row + static_cast<size_t>(width) * 4 > raw.size()) break;
            std::memcpy(&shape_bgra_[static_cast<size_t>(y) * width * 4], &raw[row],
                        static_cast<size_t>(width) * 4);
        }
    } else if (info.Type == DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR) {
        shape_bgra_.assign(static_cast<size_t>(width) * height * 4, 0);
        for (int32_t y = 0; y < height; ++y) {
            const size_t row = static_cast<size_t>(y) * pitch;
            if (row + static_cast<size_t>(width) * 4 > raw.size()) break;
            for (int32_t x = 0; x < width; ++x) {
                const uint8_t* src = &raw[row + static_cast<size_t>(x) * 4];
                uint8_t* dst = &shape_bgra_[(static_cast<size_t>(y) * width + x) * 4];
                // The alpha byte carries the mask: zero means "replace", any
                // other value means "invert what is underneath".
                if (src[3] == 0) {
                    dst[0] = src[0]; dst[1] = src[1]; dst[2] = src[2]; dst[3] = 255;
                } else {
                    dst[0] = dst[1] = dst[2] = 0; dst[3] = 255;
                }
            }
        }
    } else {
        return false;
    }

    shape_width_ = width;
    shape_height_ = height;
    shape_id_++;
    shape_pending_ = true;
    return true;
}

bool Duplicator::TakeCursorShape(std::vector<uint8_t>* bgra, int32_t* width, int32_t* height,
                                 int32_t* hot_x, int32_t* hot_y, uint32_t* shape_id) {
    if (!shape_pending_) return false;
    shape_pending_ = false;
    *bgra = shape_bgra_;
    *width = shape_width_;
    *height = shape_height_;
    *hot_x = shape_hot_x_;
    *hot_y = shape_hot_y_;
    *shape_id = shape_id_;
    return true;
}

void Duplicator::CursorPosition(int32_t* x, int32_t* y, bool* visible) const {
    *x = cursor_x_;
    *y = cursor_y_;
    *visible = cursor_visible_;
}

}  // namespace allshare
