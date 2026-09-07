/*
 * ALL SHARE — native screen capture and hardware encode for Windows.
 *
 * This is the C ABI the Go agent binds to. Everything above it is platform
 * independent; everything below it is Direct3D, DXGI and Media Foundation.
 *
 * The pipeline never touches system memory:
 *
 *   Desktop Duplication  →  BGRA texture in VRAM
 *   Video Processor      →  NV12 texture, scaled, still in VRAM
 *   Media Foundation MFT →  encoded bitstream
 *
 * The encoder is reached through Media Foundation rather than NVENC, AMF or
 * oneVPL directly. Those give slightly more control, but each needs its own SDK,
 * its own code path and its own bugs, while the vendor-supplied MFT wraps the
 * same silicon behind one interface that works on NVIDIA, Intel and AMD alike.
 * For a product that must run on whatever GPU the user happens to own, one
 * well-tuned path beats three partly-tested ones.
 *
 * Threading: a session owns a dedicated capture thread, because Desktop
 * Duplication and an asynchronous MFT both want a single COM apartment and
 * predictable timing. Go polls; it never calls into Direct3D itself.
 */
#ifndef ALLSHARE_CAPTURE_H
#define ALLSHARE_CAPTURE_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Codec identifiers. */
#define AS_CODEC_H264 0
#define AS_CODEC_H265 1

/* Quality presets, mirroring the client's own wording. */
#define AS_PRESET_BALANCED 0
#define AS_PRESET_GAMING   1
#define AS_PRESET_DESKTOP  2

/* Return codes. Negative values are errors. */
#define AS_OK            0
#define AS_TIMEOUT       1  /* no new frame within the timeout, which is normal */
#define AS_ERR_GENERIC  (-1)
#define AS_ERR_LOST     (-2) /* the desktop went away; the caller should retry */
#define AS_ERR_CLOSED   (-3)

#define AS_MAX_NAME 128
#define AS_MAX_ERROR 512

typedef struct as_monitor {
    int32_t id;
    char    name[AS_MAX_NAME];
    int32_t width;
    int32_t height;
    int32_t x;
    int32_t y;
    int32_t primary;
    int32_t refresh_hz;
    int32_t scale_percent;
} as_monitor;

typedef struct as_capability {
    int32_t codec;
    char    encoder[AS_MAX_NAME];
    int32_t hardware;
    int32_t max_width;
    int32_t max_height;
    int32_t max_fps;
    /* Space-separated H.264 profile-level-id values this encoder can produce,
     * best first. Empty for codecs where the concept does not apply. */
    char    profiles[AS_MAX_NAME];
} as_capability;

typedef struct as_open_options {
    int32_t codec;
    int32_t width;         /* 0 means the monitor's native size */
    int32_t height;
    int32_t fps;
    int32_t bitrate;       /* bits per second */
    int32_t monitor_id;
    int32_t exclude_cursor;/* keep the pointer out of the video */
    int32_t preset;
} as_open_options;

typedef struct as_info {
    int32_t codec;
    char    encoder[AS_MAX_NAME];
    char    backend[AS_MAX_NAME];
    int32_t hardware;
    int32_t width;
    int32_t height;
    int32_t fps;
    int32_t monitor_id;
    int32_t cursor_embedded;
    char    profile[32];
} as_info;

typedef struct as_frame {
    const uint8_t* data;
    int32_t        size;
    int32_t        keyframe;
    int64_t        capture_time_us; /* microseconds since the Unix epoch */
    int32_t        encode_us;
    int32_t        width;
    int32_t        height;
} as_frame;

typedef struct as_cursor {
    uint32_t       shape_id;
    const uint8_t* bgra;     /* null when only the position changed */
    int32_t        width;
    int32_t        height;
    int32_t        hot_x;
    int32_t        hot_y;
    int32_t        x;        /* pixels, relative to the captured surface */
    int32_t        y;
    int32_t        visible;
} as_cursor;

typedef struct as_stats {
    float   capture_fps;
    float   encode_fps;
    float   capture_ms;
    float   encode_ms;
    float   queue_ms;
    float   qp;
    int32_t idle_skipped;
    int32_t width;
    int32_t height;
    int32_t dropped;
    int32_t reacquires;
} as_stats;

typedef struct as_capture as_capture;
typedef struct as_audio as_audio;

/* One encoded Opus packet. */
typedef struct as_audio_frame {
    const uint8_t* data;
    int32_t        size;
    int32_t        duration_us;
    int64_t        capture_time_us;
} as_audio_frame;

/* Process-wide setup. Safe to call more than once. */
int32_t as_initialize(char* err, int32_t err_len);
void    as_shutdown(void);

/* Enumerate displays. Returns the count written, or a negative error. */
int32_t as_enumerate_monitors(as_monitor* out, int32_t max_count);

/* Enumerate encoders this machine can actually use, best first. */
int32_t as_query_capabilities(as_capability* out, int32_t max_count);

/* Open a capture session. Returns null and fills err on failure. */
as_capture* as_open(const as_open_options* options, char* err, int32_t err_len);

/*
 * Wait for the next encoded frame.
 *
 * Returns AS_OK with out filled, AS_TIMEOUT when nothing changed on screen
 * within the timeout, or a negative error. The returned pointer stays valid
 * until the next call on the same session.
 */
int32_t as_next_frame(as_capture* session, int32_t timeout_ms, as_frame* out);

/* Fetch a pending cursor update. Returns 1 when one was written, else 0. */
int32_t as_next_cursor(as_capture* session, as_cursor* out);

int32_t as_set_bitrate(as_capture* session, int32_t bits_per_second);
int32_t as_set_framerate(as_capture* session, int32_t fps);
int32_t as_set_resolution(as_capture* session, int32_t width, int32_t height);
int32_t as_set_monitor(as_capture* session, int32_t monitor_id);
int32_t as_set_preset(as_capture* session, int32_t preset);
void    as_request_keyframe(as_capture* session);

/* Record the newest input sequence applied, so frames can be tagged with it. */
void    as_note_input(as_capture* session, uint32_t sequence);

int32_t as_get_info(as_capture* session, as_info* out);
int32_t as_get_stats(as_capture* session, as_stats* out);
void    as_close(as_capture* session);

/* ---------------------------------------------------------------------------
 * Audio
 *
 * Captured with WASAPI loopback on the default playback device and encoded as
 * Opus, which is the only audio codec every browser decodes over WebRTC.
 *
 * Audio is a separate handle from video on purpose: sound should keep playing
 * across a display change or a capture restart, and a user who mutes should
 * stop the capture entirely rather than encode sound nobody is listening to.
 * ------------------------------------------------------------------------- */

/* Reports whether this build has audio support compiled in. */
int32_t as_audio_available(void);

/* Opens system audio capture. Returns null and fills err on failure. */
as_audio* as_audio_open(int32_t bitrate, char* err, int32_t err_len);

/* Takes the next encoded packet. Returns 1 when one was written, else 0.
 * The returned pointer stays valid until the next call on the same handle. */
int32_t as_audio_next(as_audio* handle, as_audio_frame* out);

void    as_audio_close(as_audio* handle);

#ifdef __cplusplus
}
#endif

#endif /* ALLSHARE_CAPTURE_H */
