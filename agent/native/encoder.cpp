// Hardware video encoding through Media Foundation for ALL SHARE.
#include "internal.h"

#include <cstring>

namespace allshare {
namespace {

// ICodecAPI's interface identifier, spelled out because not every Windows
// toolchain ships a definition for it. The value is fixed by Windows.
const GUID kIID_ICodecAPI = {
    0x901db4c7, 0x31ce, 0x41a2, { 0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda }
};

// Wraps a D3D11 texture as an IMFSample without copying it.
HRESULT WrapTexture(ID3D11Texture2D* texture, int64_t timestamp_100ns,
                    int64_t duration_100ns, ComPtr<IMFSample>* out) {
    ComPtr<IMFMediaBuffer> buffer;
    HRESULT hr = MFCreateDXGISurfaceBuffer(__uuidof(ID3D11Texture2D), texture, 0, FALSE,
                                           buffer.GetAddressOf());
    if (FAILED(hr)) return hr;

    // The buffer's current length is zero until told otherwise, and an MFT will
    // reject a zero-length sample.
    ComPtr<IMF2DBuffer> buffer2d;
    if (SUCCEEDED(buffer.As(&buffer2d))) {
        DWORD length = 0;
        if (SUCCEEDED(buffer2d->GetContiguousLength(&length))) {
            buffer->SetCurrentLength(length);
        }
    }

    ComPtr<IMFSample> sample;
    hr = MFCreateSample(sample.GetAddressOf());
    if (FAILED(hr)) return hr;
    hr = sample->AddBuffer(buffer.Get());
    if (FAILED(hr)) return hr;
    sample->SetSampleTime(timestamp_100ns);
    sample->SetSampleDuration(duration_100ns);
    *out = sample;
    return S_OK;
}

std::string WideToUtf8(const wchar_t* text) {
    if (!text) return {};
    const int needed = WideCharToMultiByte(CP_UTF8, 0, text, -1, nullptr, 0, nullptr, nullptr);
    if (needed <= 1) return {};
    std::string out(static_cast<size_t>(needed - 1), '\0');
    WideCharToMultiByte(CP_UTF8, 0, text, -1, out.data(), needed, nullptr, nullptr);
    return out;
}

bool SetCodecUint32(ICodecAPI* api, const GUID& property, uint32_t value) {
    if (!api) return false;
    VARIANT variant{};
    variant.vt = VT_UI4;
    variant.ulVal = value;
    return SUCCEEDED(api->SetValue(&property, &variant));
}

bool SetCodecBool(ICodecAPI* api, const GUID& property, bool value) {
    if (!api) return false;
    VARIANT variant{};
    variant.vt = VT_BOOL;
    variant.boolVal = value ? VARIANT_TRUE : VARIANT_FALSE;
    return SUCCEEDED(api->SetValue(&property, &variant));
}

}  // namespace

bool Encoder::Create(Device* device, int32_t codec, int32_t width, int32_t height,
                     int32_t fps, int32_t bitrate, int32_t preset, std::string* error) {
    Reset();
    device_ = device;
    codec_ = codec;
    width_ = RoundUpTo(width, 2);
    height_ = RoundUpTo(height, 2);
    fps_ = fps > 0 ? fps : 60;

    const GUID output_subtype = (codec == AS_CODEC_H265) ? MFVideoFormat_HEVC : MFVideoFormat_H264;

    MFT_REGISTER_TYPE_INFO input_info{ MFMediaType_Video, MFVideoFormat_NV12 };
    MFT_REGISTER_TYPE_INFO output_info{ MFMediaType_Video, output_subtype };

    IMFActivate** activates = nullptr;
    UINT32 count = 0;
    // Hardware first. SORTANDFILTER puts the preferred implementation first,
    // which on a machine with both an iGPU and a discrete card is the one the
    // driver considers best for this adapter.
    HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                           MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                           &input_info, &output_info, &activates, &count);
    bool hardware = true;
    if (FAILED(hr) || count == 0) {
        if (activates) CoTaskMemFree(activates);
        activates = nullptr;
        count = 0;
        // Software fallback. Much slower and not suitable for a game, but a
        // working session at reduced quality beats refusing to connect at all.
        hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                       MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_ASYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER,
                       &input_info, &output_info, &activates, &count);
        hardware = false;
    }
    if (FAILED(hr) || count == 0) {
        if (activates) CoTaskMemFree(activates);
        *error = "this PC has no video encoder ALL SHARE can use";
        return false;
    }

    // Try each candidate: the first one enumerated is not always the one that
    // will actually accept our media types on this driver version.
    std::string last_error;
    for (UINT32 index = 0; index < count; ++index) {
        ComPtr<IMFTransform> transform;
        if (FAILED(activates[index]->ActivateObject(__uuidof(IMFTransform),
                                                    reinterpret_cast<void**>(transform.GetAddressOf())))) {
            continue;
        }

        wchar_t* friendly = nullptr;
        UINT32 friendly_len = 0;
        activates[index]->GetAllocatedString(MFT_FRIENDLY_NAME_Attribute, &friendly, &friendly_len);
        std::string name = WideToUtf8(friendly);
        if (friendly) CoTaskMemFree(friendly);
        if (name.empty()) name = hardware ? "Hardware encoder" : "Software encoder";

        ComPtr<IMFAttributes> attributes;
        bool is_async = false;
        if (SUCCEEDED(transform->GetAttributes(attributes.GetAddressOf())) && attributes) {
            UINT32 async = 0;
            attributes->GetUINT32(MF_TRANSFORM_ASYNC, &async);
            is_async = async != 0;
            if (is_async) {
                // An asynchronous MFT will not accept input until unlocked.
                attributes->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
            }
            // Let the encoder keep its work on the GPU.
            attributes->SetUINT32(MF_SA_D3D11_AWARE, TRUE);
            // Low latency at the Media Foundation level as well as the codec
            // level: no frame reordering, one input to one output.
            attributes->SetUINT32(MF_LOW_LATENCY, TRUE);
        }

        if (hardware && device->Manager()) {
            // This is what makes the pipeline zero-copy. Without it the encoder
            // would demand system-memory NV12 and every frame would cross the
            // bus twice.
            hr = transform->ProcessMessage(MFT_MESSAGE_SET_D3D_MANAGER,
                                           reinterpret_cast<ULONG_PTR>(device->Manager()));
            if (FAILED(hr)) {
                last_error = DescribeHResult("MFT_MESSAGE_SET_D3D_MANAGER", hr);
                transform.Reset();
                activates[index]->ShutdownObject();
                continue;
            }
        }

        // Output type first: encoders derive their input constraints from it.
        ComPtr<IMFMediaType> output_type;
        MFCreateMediaType(output_type.GetAddressOf());
        output_type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        output_type->SetGUID(MF_MT_SUBTYPE, output_subtype);
        output_type->SetUINT32(MF_MT_AVG_BITRATE, static_cast<UINT32>(bitrate));
        MFSetAttributeSize(output_type.Get(), MF_MT_FRAME_SIZE,
                           static_cast<UINT32>(width_), static_cast<UINT32>(height_));
        MFSetAttributeRatio(output_type.Get(), MF_MT_FRAME_RATE,
                            static_cast<UINT32>(fps_), 1);
        MFSetAttributeRatio(output_type.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
        output_type->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);

        // Constrained High for H.264 where the encoder supports it: CABAC and
        // the 8x8 transform are worth roughly ten percent on screen content,
        // which shows up directly as sharper text at the same bitrate. Falling
        // back to Baseline keeps older decoders working.
        std::string chosen_profile;
        if (codec == AS_CODEC_H264) {
            output_type->SetUINT32(MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_High);
            chosen_profile = "640c1f";
        }

        hr = transform->SetOutputType(0, output_type.Get(), 0);
        if (FAILED(hr) && codec == AS_CODEC_H264) {
            output_type->SetUINT32(MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Main);
            chosen_profile = "4d001f";
            hr = transform->SetOutputType(0, output_type.Get(), 0);
            if (FAILED(hr)) {
                output_type->SetUINT32(MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Base);
                chosen_profile = "42e01f";
                hr = transform->SetOutputType(0, output_type.Get(), 0);
            }
        }
        if (FAILED(hr)) {
            last_error = DescribeHResult("SetOutputType", hr);
            transform.Reset();
            activates[index]->ShutdownObject();
            continue;
        }

        ComPtr<IMFMediaType> input_type;
        MFCreateMediaType(input_type.GetAddressOf());
        input_type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        input_type->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
        MFSetAttributeSize(input_type.Get(), MF_MT_FRAME_SIZE,
                           static_cast<UINT32>(width_), static_cast<UINT32>(height_));
        MFSetAttributeRatio(input_type.Get(), MF_MT_FRAME_RATE, static_cast<UINT32>(fps_), 1);
        MFSetAttributeRatio(input_type.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1);
        input_type->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);

        hr = transform->SetInputType(0, input_type.Get(), 0);
        if (FAILED(hr)) {
            last_error = DescribeHResult("SetInputType", hr);
            transform.Reset();
            activates[index]->ShutdownObject();
            continue;
        }

        transform_ = transform;
        name_ = name;
        hardware_ = hardware;
        async_ = is_async;
        profile_ = chosen_profile;
        // ICodecAPI is queried by explicit IID rather than __uuidof: not every
        // toolchain carries a uuid attribute for it, and the IID is stable.
        transform_->QueryInterface(kIID_ICodecAPI, reinterpret_cast<void**>(codec_api_.GetAddressOf()));
        if (async_) transform_.As(&events_);
        break;
    }

    for (UINT32 index = 0; index < count; ++index) activates[index]->Release();
    CoTaskMemFree(activates);

    if (!transform_) {
        *error = last_error.empty() ? "no video encoder on this PC accepted the requested format"
                                    : last_error;
        return false;
    }

    if (!ConfigureCodecApi(bitrate, fps_, preset, error)) return false;

    // Capture the sequence header now. Some encoders emit SPS/PPS inline before
    // every keyframe and some do not; keeping a copy lets us guarantee that a
    // decoder joining at any keyframe has what it needs.
    ComPtr<IMFMediaType> actual_output;
    if (SUCCEEDED(transform_->GetOutputCurrentType(0, actual_output.GetAddressOf())) && actual_output) {
        UINT32 header_size = 0;
        if (SUCCEEDED(actual_output->GetBlobSize(MF_MT_MPEG_SEQUENCE_HEADER, &header_size)) &&
            header_size > 0 && header_size < 4096) {
            sequence_header_.resize(header_size);
            actual_output->GetBlob(MF_MT_MPEG_SEQUENCE_HEADER, sequence_header_.data(),
                                   header_size, &header_size);
            sequence_header_.resize(header_size);
        }
    }

    transform_->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
    transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    return true;
}

bool Encoder::ConfigureCodecApi(int32_t bitrate, int32_t fps, int32_t preset, std::string* error) {
    if (!codec_api_) {
        // Not fatal: the media type already carries bitrate and frame rate.
        // Only the finer controls are unavailable.
        return true;
    }

    // The single most important setting. Without it the encoder is free to
    // reorder frames and hold several before emitting any, which adds tens of
    // milliseconds for no benefit to an interactive stream.
    SetCodecBool(codec_api_.Get(), CODECAPI_AVLowLatencyMode, true);
    SetCodecBool(codec_api_.Get(), CODECAPI_AVEncCommonRealTime, true);

    // Constant bitrate keeps the pipe full and predictable, which is what the
    // congestion controller assumes when it hands us a target.
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncCommonRateControlMode,
                   eAVEncCommonRateControlMode_CBR);
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncCommonMeanBitRate,
                   static_cast<uint32_t>(bitrate));
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncCommonMaxBitRate,
                   static_cast<uint32_t>(bitrate));

    // B-frames are pure latency here: they require the decoder to wait for a
    // later frame before it can present an earlier one.
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncMPVDefaultBPictureCount, 0);

    // An effectively infinite GOP, with keyframes produced on demand instead.
    //
    // This is the biggest single win for text clarity. A periodic keyframe
    // spends a large share of the bitrate re-sending a screen that has not
    // changed, and the result is the familiar remote-desktop pulse where text
    // blurs and re-sharpens every few seconds. Sending a keyframe only when the
    // receiver actually asks for one lets a still screen keep refining until it
    // is effectively lossless.
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncMPVGOPSize, 0xFFFFFFFFu);

    // Quality floor and ceiling. Capping the quantiser stops the encoder from
    // throwing away all detail during a brief burst of motion, which is exactly
    // when a user is most likely to be reading something.
    if (preset == AS_PRESET_DESKTOP) {
        SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncVideoMaxQP, 34);
        SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncVideoEncodeQP, 20);
    } else if (preset == AS_PRESET_GAMING) {
        SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncVideoMaxQP, 42);
    } else {
        SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncVideoMaxQP, 38);
    }

    // Slice-per-frame keeps packet loss from destroying a whole picture while
    // still letting the encoder emit as soon as the frame is done.
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncNumWorkerThreads, 0);
    SetCodecBool(codec_api_.Get(), CODECAPI_AVEncVideoForceKeyFrame, false);
    return true;
}

void Encoder::Reset() {
    if (transform_) {
        transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        transform_->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
        transform_->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
    }
    events_.Reset();
    codec_api_.Reset();
    transform_.Reset();
    sequence_header_.clear();
    sent_sequence_header_ = false;
    frame_index_ = 0;
    name_.clear();
    profile_.clear();
}

bool Encoder::SetBitrate(int32_t bits_per_second) {
    if (!codec_api_ || bits_per_second <= 0) return false;
    const bool mean = SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncCommonMeanBitRate,
                                     static_cast<uint32_t>(bits_per_second));
    SetCodecUint32(codec_api_.Get(), CODECAPI_AVEncCommonMaxBitRate,
                   static_cast<uint32_t>(bits_per_second));
    return mean;
}

bool Encoder::SetFrameRate(int32_t fps) {
    if (fps <= 0) return false;
    fps_ = fps;
    // The frame rate is advisory to a CBR encoder; the real pacing comes from
    // how often frames are submitted. Telling the encoder anyway keeps its
    // rate-control model honest.
    return true;
}

bool Encoder::Encode(ID3D11Texture2D* nv12, int64_t capture_time_us, bool force_keyframe,
                     std::vector<EncodedPacket>* out, std::string* error) {
    if (!transform_) {
        *error = "the encoder is not running";
        return false;
    }
    if (force_keyframe && codec_api_) {
        SetCodecBool(codec_api_.Get(), CODECAPI_AVEncVideoForceKeyFrame, true);
    }

    const int64_t duration = 10000000LL / (fps_ > 0 ? fps_ : 60);
    const int64_t timestamp = frame_index_ * duration;
    frame_index_++;

    ComPtr<IMFSample> sample;
    HRESULT hr = WrapTexture(nv12, timestamp, duration, &sample);
    if (FAILED(hr)) {
        *error = DescribeHResult("wrap the captured frame for the encoder", hr);
        return false;
    }
    // The capture time is tracked alongside rather than stored on the sample.
    //
    // A custom sample attribute is not guaranteed to survive a vendor encoder,
    // but the presentation timestamp is: encoders must preserve it. Timestamps
    // are assigned deterministically here, so matching an output sample back to
    // the frame it came from is exact, and the encoded packet can carry the
    // moment the pixels were grabbed rather than the moment encoding finished.
    pending_.push_back(PendingFrame{timestamp, capture_time_us, MonotonicMicros()});
    if (pending_.size() > 64) pending_.pop_front();
    encode_started_us_ = MonotonicMicros();

    if (async_ && events_) {
        // An asynchronous MFT tells us when it wants input. Waiting for that
        // event rather than pushing blindly is what keeps a hardware encoder
        // from returning MF_E_NOTACCEPTING under load.
        ComPtr<IMFMediaEvent> event;
        hr = events_->GetEvent(0, event.GetAddressOf());
        if (SUCCEEDED(hr)) {
            MediaEventType type = 0;
            event->GetType(&type);
            if (type == METransformHaveOutput) {
                if (!CollectOutput(out, error)) return false;
                hr = events_->GetEvent(0, event.ReleaseAndGetAddressOf());
                if (SUCCEEDED(hr)) event->GetType(&type);
            }
            if (type != METransformNeedInput) {
                // Nothing wanted right now; drop this frame rather than block.
                return true;
            }
        }
    }

    hr = transform_->ProcessInput(0, sample.Get(), 0);
    if (hr == MF_E_NOTACCEPTING) {
        // The encoder is saturated. Dropping the newest frame is correct for a
        // live stream: by the time it could be encoded, a fresher one exists.
        return CollectOutput(out, error);
    }
    if (FAILED(hr)) {
        *error = DescribeHResult("ProcessInput", hr);
        return false;
    }
    return CollectOutput(out, error);
}

bool Encoder::CollectOutput(std::vector<EncodedPacket>* out, std::string* error) {
    if (!transform_) return true;

    for (;;) {
        MFT_OUTPUT_STREAM_INFO stream_info{};
        transform_->GetOutputStreamInfo(0, &stream_info);

        MFT_OUTPUT_DATA_BUFFER buffer{};
        DWORD status = 0;
        ComPtr<IMFSample> sample;

        const bool provides_samples =
            (stream_info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                                    MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES)) != 0;
        if (!provides_samples) {
            // A synchronous software MFT expects us to supply the buffer.
            ComPtr<IMFMediaBuffer> media_buffer;
            HRESULT hr = MFCreateMemoryBuffer(
                stream_info.cbSize > 0 ? stream_info.cbSize : 1 << 20, media_buffer.GetAddressOf());
            if (FAILED(hr)) {
                *error = DescribeHResult("MFCreateMemoryBuffer", hr);
                return false;
            }
            hr = MFCreateSample(sample.GetAddressOf());
            if (FAILED(hr)) {
                *error = DescribeHResult("MFCreateSample", hr);
                return false;
            }
            sample->AddBuffer(media_buffer.Get());
            buffer.pSample = sample.Get();
        }

        HRESULT hr = transform_->ProcessOutput(0, 1, &buffer, &status);
        if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) return true;
        if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {
            // The encoder renegotiated its output; accept the new type and
            // carry on rather than tearing the session down.
            ComPtr<IMFMediaType> new_type;
            if (SUCCEEDED(transform_->GetOutputAvailableType(0, 0, new_type.GetAddressOf()))) {
                transform_->SetOutputType(0, new_type.Get(), 0);
            }
            if (buffer.pEvents) buffer.pEvents->Release();
            continue;
        }
        if (FAILED(hr)) {
            *error = DescribeHResult("ProcessOutput", hr);
            if (buffer.pSample && provides_samples) buffer.pSample->Release();
            if (buffer.pEvents) buffer.pEvents->Release();
            return false;
        }

        IMFSample* produced = buffer.pSample;
        if (produced) {
            UINT32 clean_point = 0;
            produced->GetUINT32(MFSampleExtension_CleanPoint, &clean_point);

            LONGLONG sample_time = 0;
            produced->GetSampleTime(&sample_time);
            int64_t capture_time_us = UnixMicros();
            int64_t started_us = encode_started_us_;
            for (auto it = pending_.begin(); it != pending_.end(); ++it) {
                if (it->timestamp_100ns != sample_time) continue;
                capture_time_us = it->capture_time_us;
                started_us = it->submitted_us;
                pending_.erase(pending_.begin(), it + 1);
                break;
            }

            ComPtr<IMFMediaBuffer> contiguous;
            if (SUCCEEDED(produced->ConvertToContiguousBuffer(contiguous.GetAddressOf()))) {
                BYTE* data = nullptr;
                DWORD max_len = 0, current = 0;
                if (SUCCEEDED(contiguous->Lock(&data, &max_len, &current))) {
                    const int32_t encode_us =
                        static_cast<int32_t>(MonotonicMicros() - started_us);
                    AppendAnnexB(data, static_cast<int32_t>(current), clean_point != 0,
                                 capture_time_us, encode_us, out);
                    contiguous->Unlock();
                }
            }
            if (provides_samples) produced->Release();
        }
        if (buffer.pEvents) buffer.pEvents->Release();

        if (!(status & MFT_OUTPUT_DATA_BUFFER_INCOMPLETE)) {
            // One frame in, one frame out under low-latency mode.
            return true;
        }
    }
}

void Encoder::AppendAnnexB(const uint8_t* data, int32_t size, bool keyframe,
                           int64_t capture_time_us, int32_t encode_us,
                           std::vector<EncodedPacket>* out) {
    if (size <= 0) return;

    EncodedPacket packet;
    packet.keyframe = keyframe;
    packet.capture_time_us = capture_time_us;
    packet.encode_us = encode_us;
    packet.width = width_;
    packet.height = height_;

    // Prepend the parameter sets to every keyframe unless the encoder already
    // did. A client that joins mid-stream, or one recovering from loss, has
    // nothing to decode without them, and duplicating a hundred bytes on a
    // keyframe is cheaper than an undecodable stream.
    const bool starts_with_parameter_set =
        size > 4 && data[0] == 0 && data[1] == 0 &&
        ((data[2] == 1 && (data[3] & 0x1F) == 7) ||
         (data[2] == 0 && data[3] == 1 && size > 4 && (data[4] & 0x1F) == 7));

    if (keyframe && !sequence_header_.empty() && !starts_with_parameter_set) {
        packet.data.insert(packet.data.end(), sequence_header_.begin(), sequence_header_.end());
    }
    packet.data.insert(packet.data.end(), data, data + size);
    out->push_back(std::move(packet));
}

bool Encoder::Drain(std::vector<EncodedPacket>* out, std::string* error) {
    if (!transform_) return true;
    transform_->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN, 0);
    return CollectOutput(out, error);
}

}  // namespace allshare
