#define _POSIX_C_SOURCE 200809L
#include <unistd.h>
#include <time.h>
#include <rk_type.h>
#include <rk_mpi_venc.h>
#include <rk_mpi_sys.h>
#include <string.h>
#include <stdlib.h>
#include <rk_debug.h>
#include <malloc.h>
#include <stdbool.h>
#include <rk_mpi_mb.h>
#include <fcntl.h>
#include <linux/videodev2.h>
#include <sys/ioctl.h>
#include <errno.h>
#include <unistd.h>
#include <stdatomic.h>
#include <stdint.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <rk_mpi_mmz.h>
#include <pthread.h>
#include <assert.h>
#include <sys/un.h>
#include <sys/socket.h>
#include "video.h"
#include "ctrl.h"
#include "log.h"

#define VIDEO_DEV "/dev/video0"
#define SUB_DEV "/dev/v4l-subdev2"
#define SLEEP_MODE_FILE "/sys/devices/platform/ff470000.i2c/i2c-4/4-000f/sleep_mode"

#define RK_ALIGN(x, a) (((x) + (a)-1) & ~((a)-1))
#define RK_ALIGN_2(x) RK_ALIGN(x, 2)
#define RK_ALIGN_16(x) RK_ALIGN(x, 16)
#define RK_ALIGN_32(x) RK_ALIGN(x, 32)

int sub_dev_fd = -1;
#define VENC_CHANNEL 0
MB_POOL memPool = MB_INVALID_POOLID;

bool sleep_mode_available = false;
bool should_exit = false;
float quality_factor = 1.0f;
int codec_type = 0;

static void *venc_read_stream(void *arg);

RK_U64 get_us()
{
    struct timespec time = {0, 0};
    clock_gettime(CLOCK_MONOTONIC, &time);
    return (RK_U64)time.tv_sec * 1000000 + (RK_U64)time.tv_nsec / 1000; /* microseconds */
}

static void ensure_sleep_mode_disabled()
{
    if (!sleep_mode_available)
    {
        return;
    }

    int fd = open(SLEEP_MODE_FILE, O_RDWR);
    if (fd < 0)
    {
        log_error("Failed to open sleep mode file: %s", strerror(errno));
        return;
    }
    lseek(fd, 0, SEEK_SET);
    char buffer[1];
    read(fd, buffer, 1);
    if (buffer[0] == '0') {
        close(fd);
        return;
    }
    log_warn("HDMI sleep mode is not disabled, disabling it");
    lseek(fd, 0, SEEK_SET);
    write(fd, "0", 1);
    close(fd);

    usleep(1000); // give some time to the system to disable the sleep mode
    return;
}

static void detect_sleep_mode()
{
    if (access(SLEEP_MODE_FILE, F_OK) != 0) {
        sleep_mode_available = false;
        return;
    }
    sleep_mode_available = true;
    ensure_sleep_mode_disabled();
}

double calculate_bitrate(float bitrate_factor, int width, int height)
{
    const int32_t base_bitrate_high = 4000;
    const int32_t base_bitrate_low = 512;

    double pixels = (double)width * height;
    double ref_pixels = 1920.0 * 1080.0;

    double scale_factor = pixels / ref_pixels;

    int32_t base_bitrate = base_bitrate_low + (int32_t)((base_bitrate_high - base_bitrate_low) * bitrate_factor);

    int32_t bitrate = (int32_t)(base_bitrate * scale_factor);

    const int32_t min_bitrate = 200;
    if (bitrate < min_bitrate)
    {
        bitrate = min_bitrate;
    }

    return bitrate;
}

// The TC358749 bridge always converts to BT.601 limited-range YCbCr for the
// YUV 4:2:2 bus format (tc35874x.c tc35874x_set_csi_color_space():
// MASK_VOUT_COLOR_601_YCBCR_LIMITED), whatever the resolution. Measured
// against a test image: decoded colors match the BT.601 matrix within 0.5
// code values, the BT.709 matrix is off by ~8. The content is an sRGB desktop,
// so only the matrix is 601: tag BT.709 (~sRGB) primaries/transfer with the
// BT.601 matrix, limited range. Tagging only; the conversion is the bridge's.
static void set_v4l2_yuv_colorimetry(struct v4l2_pix_format_mplane *pix)
{
    pix->colorspace = V4L2_COLORSPACE_REC709;
    pix->ycbcr_enc = V4L2_YCBCR_ENC_601;
    pix->quantization = V4L2_QUANTIZATION_LIM_RANGE;
    pix->xfer_func = V4L2_XFER_FUNC_709;
}

static void configure_vui_video_signal(VENC_VUI_VIDEO_SIGNAL_S *video_signal)
{
    memset(video_signal, 0, sizeof(VENC_VUI_VIDEO_SIGNAL_S));
    video_signal->video_signal_type_present_flag = 1;
    video_signal->video_format = 5; // unspecified
    video_signal->video_full_range_flag = 0; // YUV video levels: black=16, white=235
    video_signal->colour_description_present_flag = 1;
    video_signal->colour_primaries = 1; // BT.709 (sRGB desktop)
    video_signal->transfer_characteristics = 1; // BT.709
    video_signal->matrix_coefficients = 6; // BT.601 (SMPTE 170M), as converted by the bridge
}

// Encoder tunables. Defaults are compiled in, overridable via env (read once in
// video_init; inherited by the native subprocess); video_set_encoder_params()
// replaces them at runtime (in memory only), live where rockit allows it.
// video_reset_encoder_params() returns to the defaults when the last session
// ends and on quality-factor changes, so overrides never outlive a client.
//
// A static desktop at a low QP makes every IDR large (~150-350 KiB at 1080p),
// so a short GOP resends it several times a second. Default to a long GOP and
// rely on on-demand IDRs (video_request_keyframe) for new sessions and
// receiver PLI/FIR.
#define VENC_PARAMS_COMPILED_DEFAULTS \
    {                                 \
        .min_qp = 18,                 \
        .max_qp = 42,                 \
        .gop_seconds = 60.0,          \
        .aq = false,                  \
        .max_fps = 0,                 \
        .bitrate_kbps = 0,            \
        .max_bitrate_kbps = 0,        \
        .still_qp = 8,                \
    }
static jetkvm_venc_params_t venc_params = VENC_PARAMS_COMPILED_DEFAULTS;
// Compiled defaults with env overrides applied (venc_params_load_env).
static jetkvm_venc_params_t venc_params_defaults = VENC_PARAMS_COMPILED_DEFAULTS;
static pthread_mutex_t venc_params_mutex = PTHREAD_MUTEX_INITIALIZER;

static RK_U32 env_u32(const char *name, RK_U32 def, RK_U32 lo, RK_U32 hi)
{
    const char *s = getenv(name);
    if (s == NULL || *s == '\0')
    {
        return def;
    }
    char *end = NULL;
    unsigned long v = strtoul(s, &end, 10);
    if (*end != '\0' || v < lo || v > hi)
    {
        log_notice("ignoring invalid %s=%.32s", name, s);
        return def;
    }
    return (RK_U32)v;
}

// An invalid JETKVM_VENC_GOP_SECONDS falls back to the upstream 0.5 s cadence.
static double env_gop_seconds(double def)
{
    const char *s = getenv("JETKVM_VENC_GOP_SECONDS");
    if (s == NULL || *s == '\0')
    {
        return def;
    }
    char *end = NULL;
    double seconds = strtod(s, &end);
    if (*end != '\0' || !(seconds >= 0.5 && seconds <= 300.0))
    {
        log_notice("ignoring invalid JETKVM_VENC_GOP_SECONDS=%.32s, using 0.5", s);
        return 0.5;
    }
    return seconds;
}

static void venc_params_load_env(void)
{
    jetkvm_venc_params_t *p = &venc_params;
    p->min_qp = env_u32("JETKVM_VENC_MIN_QP", p->min_qp, 8, 40);
    p->max_qp = env_u32("JETKVM_VENC_MAX_QP", p->max_qp > p->min_qp ? p->max_qp : p->min_qp, p->min_qp, 51);
    p->gop_seconds = env_gop_seconds(p->gop_seconds);
    p->aq = env_u32("JETKVM_VENC_AQ", p->aq, 0, 1) == 1;
    p->max_fps = env_u32("JETKVM_VENC_MAX_FPS", p->max_fps, 0, 120);
    p->still_qp = env_u32("JETKVM_VENC_STILL_QP", p->still_qp, 1, 51);
    venc_params_defaults = *p;
}

static jetkvm_venc_params_t venc_get_params(void)
{
    pthread_mutex_lock(&venc_params_mutex);
    jetkvm_venc_params_t p = venc_params;
    pthread_mutex_unlock(&venc_params_mutex);
    return p;
}

static bool venc_params_valid(const jetkvm_venc_params_t *p)
{
    return p->min_qp >= 8 && p->min_qp <= 40 &&
           p->max_qp >= p->min_qp && p->max_qp <= 51 &&
           p->gop_seconds >= 0.5 && p->gop_seconds <= 300.0 &&
           p->still_qp >= 1 && p->still_qp <= 51 &&
           p->max_fps >= 0 && p->max_fps <= 120 &&
           (p->bitrate_kbps == 0 || (p->bitrate_kbps >= 100 && p->bitrate_kbps <= 50000)) &&
           (p->max_bitrate_kbps == 0 || (p->max_bitrate_kbps >= 100 && p->max_bitrate_kbps <= 50000)) &&
           (p->bitrate_kbps == 0 || p->max_bitrate_kbps == 0 || p->max_bitrate_kbps >= p->bitrate_kbps);
}

// Frames per second actually encoded: the source rate, capped by max_fps.
static RK_U32 venc_encode_fps(RK_U32 source_fps, int max_fps)
{
    return max_fps > 0 && (RK_U32)max_fps < source_fps ? (RK_U32)max_fps : source_fps;
}

static RK_U32 venc_gop_frames(RK_U32 fps, double seconds)
{
    if (fps == 0)
    {
        return 30;
    }
    // Truncation keeps 0.5 s identical to the upstream fps / 2.
    RK_U32 gop = (RK_U32)(fps * seconds);
    return gop < 1 ? 1 : gop;
}

// Target/max in kbps: explicit overrides, else derived from the quality
// factor (overrides are cleared by video_set_quality_factor).
static void venc_bitrates(const jetkvm_venc_params_t *p, RK_U32 width, RK_U32 height,
                          RK_U32 *bitrate, RK_U32 *max_bitrate)
{
    *bitrate = p->bitrate_kbps > 0 ? (RK_U32)p->bitrate_kbps
                                   : (RK_U32)calculate_bitrate(quality_factor, width, height);
    *max_bitrate = p->max_bitrate_kbps > 0 ? (RK_U32)p->max_bitrate_kbps : *bitrate * 3 / 2;
    if (*max_bitrate < *bitrate)
    {
        *max_bitrate = *bitrate;
    }
}

static void venc_set_rc_attr(VENC_CHN_ATTR_S *stAttr, bool hevc, RK_U32 bitrate, RK_U32 max_bitrate, RK_U32 fps, RK_U32 gop)
{
    RK_U32 min_bitrate = bitrate / 2;
    if (min_bitrate < 2) min_bitrate = 2;

    if (hevc) {
        stAttr->stRcAttr.enRcMode = VENC_RC_MODE_H265VBR;
        stAttr->stRcAttr.stH265Vbr.u32BitRate = bitrate;
        stAttr->stRcAttr.stH265Vbr.u32MaxBitRate = max_bitrate;
        stAttr->stRcAttr.stH265Vbr.u32MinBitRate = min_bitrate;
        stAttr->stRcAttr.stH265Vbr.u32Gop = gop;
        stAttr->stRcAttr.stH265Vbr.u32StatTime = 2;
        stAttr->stRcAttr.stH265Vbr.u32SrcFrameRateNum = fps;
        stAttr->stRcAttr.stH265Vbr.u32SrcFrameRateDen = 1;
        stAttr->stRcAttr.stH265Vbr.fr32DstFrameRateNum = fps;
        stAttr->stRcAttr.stH265Vbr.fr32DstFrameRateDen = 1;
    } else {
        stAttr->stRcAttr.enRcMode = VENC_RC_MODE_H264VBR;
        stAttr->stRcAttr.stH264Vbr.u32BitRate = bitrate;
        stAttr->stRcAttr.stH264Vbr.u32MaxBitRate = max_bitrate;
        stAttr->stRcAttr.stH264Vbr.u32MinBitRate = min_bitrate;
        stAttr->stRcAttr.stH264Vbr.u32Gop = gop;
        stAttr->stRcAttr.stH264Vbr.u32StatTime = 2;
        stAttr->stRcAttr.stH264Vbr.u32SrcFrameRateNum = fps;
        stAttr->stRcAttr.stH264Vbr.u32SrcFrameRateDen = 1;
        stAttr->stRcAttr.stH264Vbr.fr32DstFrameRateNum = fps;
        stAttr->stRcAttr.stH264Vbr.fr32DstFrameRateDen = 1;
    }
}

static void populate_venc_attr(VENC_CHN_ATTR_S *stAttr, RK_U32 bitrate, RK_U32 max_bitrate, RK_U32 width, RK_U32 height, RK_U32 fps, RK_U32 gop)
{
    memset(stAttr, 0, sizeof(VENC_CHN_ATTR_S));

    const bool hevc = codec_type == 1;
    venc_set_rc_attr(stAttr, hevc, bitrate, max_bitrate, fps, gop);
    stAttr->stVencAttr.enType = hevc ? RK_VIDEO_ID_HEVC : RK_VIDEO_ID_AVC;
    stAttr->stVencAttr.u32Profile = hevc ? H265E_PROFILE_MAIN : H264E_PROFILE_HIGH;
    stAttr->stVencAttr.enPixelFormat = RK_FMT_YUV422_YUYV;
    stAttr->stVencAttr.u32PicWidth = width;
    stAttr->stVencAttr.u32PicHeight = height;
    stAttr->stVencAttr.u32VirWidth = RK_ALIGN_16(width);
    stAttr->stVencAttr.u32VirHeight = RK_ALIGN_16(height);
    stAttr->stVencAttr.u32StreamBufCnt = 3;
    // Twice the raw 4:2:0 size: a near-lossless refresh of a busy screen can
    // approach the raw size (internal/native/proxy.go's maxFrameSize matches).
    stAttr->stVencAttr.u32BufSize = width * height * 2;
    stAttr->stVencAttr.enMirror = MIRROR_NONE;
}

static void venc_configure_limited_range_vui(void)
{
    int32_t ret;

    if (codec_type == 1)
    {
        VENC_H265_VUI_S stH265Vui;
        memset(&stH265Vui, 0, sizeof(VENC_H265_VUI_S));
        ret = RK_MPI_VENC_GetH265Vui(VENC_CHANNEL, &stH265Vui);
        if (ret != RK_SUCCESS)
        {
            log_warn("RK_MPI_VENC_GetH265Vui failed: %#x", ret);
            return;
        }
        configure_vui_video_signal(&stH265Vui.stVuiVideoSignal);
        ret = RK_MPI_VENC_SetH265Vui(VENC_CHANNEL, &stH265Vui);
        if (ret != RK_SUCCESS)
        {
            log_warn("RK_MPI_VENC_SetH265Vui failed: %#x", ret);
            return;
        }
    }
    else
    {
        VENC_H264_VUI_S stH264Vui;
        memset(&stH264Vui, 0, sizeof(VENC_H264_VUI_S));
        ret = RK_MPI_VENC_GetH264Vui(VENC_CHANNEL, &stH264Vui);
        if (ret != RK_SUCCESS)
        {
            log_warn("RK_MPI_VENC_GetH264Vui failed: %#x", ret);
            return;
        }
        configure_vui_video_signal(&stH264Vui.stVuiVideoSignal);
        ret = RK_MPI_VENC_SetH264Vui(VENC_CHANNEL, &stH264Vui);
        if (ret != RK_SUCCESS)
        {
            log_warn("RK_MPI_VENC_SetH264Vui failed: %#x", ret);
            return;
        }
    }

    log_notice("configured VENC VUI as limited-range, BT.601 matrix, BT.709 primaries/transfer");
}

// Re-encode any frame above this size. Still refreshes set their QP per block
// (an ROI), which the re-encode cannot raise; the stream buffer and
// internal/native/proxy.go's maxFrameSize leave room above it (2 bytes per
// pixel of the largest mode).
#define VENC_SUPER_FRAME_BITS (2560u * 1024u * 8u)

#define VENC_APPLY_QP_LIMITS(p, min_qp, min_iqp, max_qp, max_iqp) \
    do                                                   \
    {                                                    \
        (p).u32MinQp = (min_qp);                         \
        (p).u32MinIQp = (min_iqp);                       \
        (p).u32FrmMinQp = (min_qp);                      \
        (p).u32FrmMinIQp = (min_iqp);                    \
        (p).u32MaxQp = (max_qp);                         \
        (p).u32FrmMaxQp = (max_qp);                      \
        (p).u32MaxIQp = (max_iqp);                       \
        (p).u32FrmMaxIQp = (max_iqp);                    \
    } while (0)

// Rockchip defaults pin the frame-level QP floor at ~26-28 and apply
// texture-adaptive QP (up to +8 on high-texture blocks, i.e. text), so desktop
// text stays soft at any bitrate. Lower the floor and flatten AQ so the VBR
// bitrate target decides quality. Used at channel creation and for live
// updates; returns false if any step failed (logged as error: the native
// process logs at ERROR by default).
static VENC_RC_PARAM2_S venc_default_rc2; // Rockchip AQ defaults of the channel
static bool venc_default_rc2_valid = false;

static bool venc_configure_rc_for_desktop(const jetkvm_venc_params_t *p, bool hevc)
{
    const RK_U32 min_qp = p->min_qp;
    // Keyframes may go down to still_qp: the still refresh is one (the
    // encoder clamps ROI QPs to the frame type's floor). Other keyframes
    // get the lower floor too; on a still desktop a PLI keyframe measured
    // ~20% larger. Moving the floor per refresh would restart rate control.
    const RK_U32 min_iqp = p->still_qp < p->min_qp ? p->still_qp : p->min_qp;
    const RK_U32 max_qp = p->max_qp;
    const RK_U32 max_iqp = max_qp >= min_qp + 2 ? max_qp - 2 : max_qp;
    bool ok = true;
    int32_t ret;

    VENC_RC_PARAM_S stRcParam;
    memset(&stRcParam, 0, sizeof(stRcParam));
    ret = RK_MPI_VENC_GetRcParam(VENC_CHANNEL, &stRcParam);
    if (ret == RK_SUCCESS)
    {
        if (hevc)
        {
            VENC_APPLY_QP_LIMITS(stRcParam.stParamH265, min_qp, min_iqp, max_qp, max_iqp);
        }
        else
        {
            VENC_APPLY_QP_LIMITS(stRcParam.stParamH264, min_qp, min_iqp, max_qp, max_iqp);
        }
        ret = RK_MPI_VENC_SetRcParam(VENC_CHANNEL, &stRcParam);
    }
    if (ret != RK_SUCCESS)
    {
        log_error("VENC QP limits not applied: %#x", ret);
        ok = false;
    }

    if (venc_default_rc2_valid)
    {
        VENC_RC_PARAM2_S stRcParam2 = venc_default_rc2;
        if (!p->aq)
        {
            // Zero QP steps => uniform QP per frame; the texture thresholds
            // (u32ThrdI/P) become irrelevant. Zeroing thresholds instead would
            // push every block into the top (+8) bucket.
            memset(stRcParam2.s32AqStepI, 0, sizeof(stRcParam2.s32AqStepI));
            memset(stRcParam2.s32AqStepP, 0, sizeof(stRcParam2.s32AqStepP));
            stRcParam2.u32RowQpDelta = 0;
            stRcParam2.u32RowIQpDelta = 0;
        }
        ret = RK_MPI_VENC_SetRcParam2(VENC_CHANNEL, &stRcParam2);
        if (ret != RK_SUCCESS)
        {
            log_error("VENC AQ setting not applied: %#x", ret);
            ok = false;
        }
    }
    else
    {
        log_error("VENC AQ setting not applied: channel defaults unknown");
        ok = false;
    }

    log_notice("configured VENC RC: qp %u-%u (I %u-%u), aq %s%s",
               min_qp, max_qp, min_iqp, max_iqp, p->aq ? "rockchip default" : "off", ok ? "" : " (with errors)");
    return ok;
}

static void venc_configure_super_frame(void)
{
    VENC_SUPERFRAME_CFG_S stSuperFrm;
    memset(&stSuperFrm, 0, sizeof(stSuperFrm));
    stSuperFrm.enSuperFrmMode = SUPERFRM_REENCODE;
    stSuperFrm.u32SuperIFrmBitsThr = VENC_SUPER_FRAME_BITS;
    stSuperFrm.u32SuperPFrmBitsThr = VENC_SUPER_FRAME_BITS;
    stSuperFrm.u32SuperBFrmBitsThr = VENC_SUPER_FRAME_BITS;
    stSuperFrm.enRcPriority = VENC_RC_PRIORITY_FRAMEBITS_FIRST;
    int32_t ret = RK_MPI_VENC_SetSuperFrameStrategy(VENC_CHANNEL, &stSuperFrm);
    if (ret != RK_SUCCESS)
    {
        log_error("RK_MPI_VENC_SetSuperFrameStrategy failed: %#x", ret);
    }
}

pthread_t *venc_read_thread = NULL;
volatile bool venc_running = false;
// Guards venc_running and venc_live. Held across channel creation and every
// call that touches the running channel from another thread (IDR requests,
// live parameter updates), so none of them races RK_MPI_VENC_DestroyChn and a
// parameter change either precedes the creation snapshot or is applied live.
static pthread_mutex_t venc_state_mutex = PTHREAD_MUTEX_INITIALIZER;

// What the running channel is configured with.
static struct
{
    jetkvm_venc_params_t params;
    bool hevc;
    RK_U32 width;
    RK_U32 height;
    RK_U32 source_fps;
    volatile RK_U32 encode_fps; // also read lock-free by the capture loop
    RK_U32 gop;       // nominal GOP length
    RK_U32 gop_nudge; // 0/1 frame added by the keyframe GOP-nudge fallback
    RK_U32 bitrate;
    RK_U32 max_bitrate;
} venc_live;

// Still-screen refresh. Encoded frames tell whether the screen changes: a
// still screen gives nearly empty P-frames. After any change, once the
// screen has settled for half a second, one keyframe is encoded at still_qp
// (a whole-frame ROI with absolute QP; keyframes may go that low, P-frames
// stay at min_qp or above), so the still picture becomes near-lossless. The
// P-frames that follow skip unchanged blocks and keep it at no cost.
// P-frames alone never get there: the encoder skips blocks whose residual is
// small, at any QP. Nothing here touches rate control (changing RC
// parameters restarts it, which costs the next moving frames quality).
//
// What counts as a change, measured on a Mac desktop at QP 10: empty
// P-frames are 150-460 bytes depending on the screen, a typed glyph 530-1000,
// a blinking caret a few hundred bytes a second. Each P-frame's cost above
// the empty-frame baseline (the smallest frame of the last one to two
// seconds) plus STILL_NOISE_BYTES feeds two decaying sums: over about three
// seconds it marks the picture dirty at STILL_DIRTY_BYTES (typing gets
// there, a caret does not), over about a tenth of a second it tells when the
// changes have stopped. A frame above a budget/8 (STILL_QUIET_MIN_BYTES at
// least) is a real change. Any other keyframe (session start, packet loss,
// GOP) counts as one too. Refreshes are at least STILL_MIN_INTERVAL_US apart,
// and a refresh above STILL_LARGE_REFRESH (a photo-heavy screen) raises the
// QP of the next ones. Encoder output thread only.
#define STILL_NOISE_BYTES 150
#define STILL_DIRTY_BYTES 2000.0
#define STILL_SETTLED_BYTES 100.0
#define STILL_QUIET_MIN_BYTES 1500
#define STILL_MIN_INTERVAL_US 1500000
#define STILL_REFRESH_MAX_FRAMES 8
// P-frames encoded while the ROI was still on belong to the refresh.
#define STILL_COOLDOWN_FRAMES 4
#define STILL_ROI_INDEX 7
#define STILL_LARGE_REFRESH (1536u * 1024u)
#define STILL_SMALL_REFRESH (768u * 1024u)
#define STILL_BOOST_STEP 6
#define STILL_BOOST_MAX 24

static struct
{
    unsigned quiet;      // frames since the last real change
    bool dirty;          // the picture changed since the last refresh
    double window;       // decaying sum of change bytes over ~3 s
    double recent;       // decaying sum over ~0.1 s
    size_t floor_now;    // smallest P-frame of this baseline window
    size_t floor_prev;   // ... and of the previous one
    unsigned floor_frames;
    unsigned refreshing; // frames encoded since the refresh started (0 = none)
    bool roi_on;         // the ROI is set (also if turning it off failed)
    unsigned cooldown;   // frames still to ignore after a refresh
    int boost;           // added to still_qp after large refreshes
    uint64_t last_refresh_us;
    bool warned;
} still;

static void still_reset(void)
{
    memset(&still, 0, sizeof(still));
    still.floor_now = SIZE_MAX;
    still.floor_prev = SIZE_MAX;
}

// Caller holds venc_state_mutex with venc_running set.
static bool still_set_roi_locked(bool enable, int qp)
{
    VENC_ROI_ATTR_S roi;
    memset(&roi, 0, sizeof(roi));
    roi.u32Index = STILL_ROI_INDEX;
    roi.bEnable = enable ? RK_TRUE : RK_FALSE;
    roi.bAbsQp = RK_TRUE;
    roi.s32Qp = qp;
    roi.bIntra = RK_FALSE;
    roi.stRect.s32X = 0;
    roi.stRect.s32Y = 0;
    roi.stRect.u32Width = venc_live.width;
    roi.stRect.u32Height = venc_live.height;
    const int32_t ret = RK_MPI_VENC_SetRoiAttr(VENC_CHANNEL, &roi);
    if (ret != RK_SUCCESS)
    {
        log_error("still refresh ROI %s failed: %#x", enable ? "on" : "off", ret);
        return false;
    }
    return true;
}

// Takes venc_state_mutex itself (defined with the keyframe paths).
static void still_request_keyframe(void);

// Called for every encoded frame with its size, after the frame went out.
static void still_on_frame(size_t len, bool idr)
{
    pthread_mutex_lock(&venc_state_mutex);
    const bool running = venc_running;
    const RK_U32 fps = venc_live.encode_fps > 0 ? venc_live.encode_fps : 60;
    const size_t budget = (size_t)venc_live.bitrate * 1000 / 8 / fps;
    const int still_qp = venc_live.params.still_qp, min_qp = venc_live.params.min_qp;
    const bool ending = still.refreshing > 0 && (idr || still.refreshing + 1 > STILL_REFRESH_MAX_FRAMES);
    // Turn the ROI off once the refresh is out; retried every frame if the
    // call fails (a lingering whole-frame absolute QP would pin every frame).
    if (still.roi_on && (ending || still.refreshing == 0) && running && still_set_roi_locked(false, 0))
    {
        still.roi_on = false;
    }
    pthread_mutex_unlock(&venc_state_mutex);

    if (still.refreshing > 0)
    {
        still.refreshing++;
        if (!ending)
        {
            return;
        }
        if (idr)
        {
            if (len > STILL_LARGE_REFRESH && still.boost < STILL_BOOST_MAX)
                still.boost += STILL_BOOST_STEP;
            else if (len < STILL_SMALL_REFRESH && still.boost > 0)
                still.boost -= STILL_BOOST_STEP;
            log_info("still refresh: keyframe %u bytes", (unsigned)len);
        }
        else if (!still.warned)
        {
            still.warned = true;
            log_notice("still refresh: the keyframe request produced no keyframe (logged once)");
        }
        still.refreshing = 0;
        still.quiet = 0;
        still.cooldown = STILL_COOLDOWN_FRAMES;
        return;
    }
    if (still.cooldown > 0 && !idr)
    {
        still.cooldown--;
        return;
    }
    if (idr)
    {
        still.quiet = 0;
        still.dirty = true;
        return;
    }

    // Empty-frame baseline over one to two seconds.
    if (len < still.floor_now)
        still.floor_now = len;
    if (++still.floor_frames >= fps)
    {
        still.floor_prev = still.floor_now;
        still.floor_now = SIZE_MAX;
        still.floor_frames = 0;
    }
    const size_t floor = still.floor_now < still.floor_prev ? still.floor_now : still.floor_prev;
    const double change = floor != SIZE_MAX && len > floor + STILL_NOISE_BYTES ? (double)(len - floor - STILL_NOISE_BYTES) : 0.0;
    still.window = still.window * (1.0 - 1.0 / (3.0 * fps)) + change;
    still.recent = still.recent * 0.9 + change;
    const size_t quiet_max = budget / 8 > STILL_QUIET_MIN_BYTES ? budget / 8 : STILL_QUIET_MIN_BYTES;
    if (len >= quiet_max)
    {
        still.quiet = 0;
        still.dirty = true;
        return;
    }
    if (still.window >= STILL_DIRTY_BYTES)
        still.dirty = true;
    if (!still.dirty || ++still.quiet < fps / 2 || still.recent >= STILL_SETTLED_BYTES || still_qp >= min_qp || !running)
    {
        return;
    }
    const uint64_t now = get_us();
    if (now - still.last_refresh_us < STILL_MIN_INTERVAL_US)
    {
        return;
    }
    const int qp = still_qp + still.boost < min_qp ? still_qp + still.boost : min_qp;
    pthread_mutex_lock(&venc_state_mutex);
    const bool started = venc_running && still_set_roi_locked(true, qp);
    if (started)
        still.roi_on = true;
    pthread_mutex_unlock(&venc_state_mutex);
    if (!started)
    {
        return;
    }
    still_request_keyframe();
    still.refreshing = 1;
    still.dirty = false;
    still.window = 0;
    still.recent = 0;
    still.last_refresh_us = now;
}

static void venc_set_running(bool running)
{
    pthread_mutex_lock(&venc_state_mutex);
    venc_running = running;
    pthread_mutex_unlock(&venc_state_mutex);
}

static int32_t venc_start(RK_U32 width, RK_U32 height, RK_U32 source_fps)
{
    int32_t ret;
    VENC_CHN_ATTR_S stAttr;

    pthread_mutex_lock(&venc_state_mutex);
    const jetkvm_venc_params_t params = venc_get_params();
    venc_live.params = params;
    venc_live.hevc = codec_type == 1;
    venc_live.width = width;
    venc_live.height = height;
    venc_live.source_fps = source_fps;
    venc_live.encode_fps = venc_encode_fps(source_fps, params.max_fps);
    venc_live.gop = venc_gop_frames(venc_live.encode_fps, params.gop_seconds);
    venc_live.gop_nudge = 0;
    venc_bitrates(&params, width, height, &venc_live.bitrate, &venc_live.max_bitrate);
    still_reset();

    populate_venc_attr(&stAttr, venc_live.bitrate, venc_live.max_bitrate, width, height,
                       venc_live.encode_fps, venc_live.gop);
    ret = RK_MPI_VENC_CreateChn(VENC_CHANNEL, &stAttr);
    if (ret != RK_SUCCESS)
    {
        RK_LOGE("error RK_MPI_VENC_CreateChn, %d", ret);
        goto out;
    }
    log_notice("configured VENC GOP: %u frames (%.1f s at %u of %u fps), bitrate %u kbps (max %u)",
               venc_live.gop, params.gop_seconds, venc_live.encode_fps, source_fps,
               venc_live.bitrate, venc_live.max_bitrate);

    venc_configure_limited_range_vui();
    venc_default_rc2_valid = RK_MPI_VENC_GetRcParam2(VENC_CHANNEL, &venc_default_rc2) == RK_SUCCESS;
    venc_configure_rc_for_desktop(&params, venc_live.hevc);
    venc_configure_super_frame();

    VENC_RECV_PIC_PARAM_S stRecvParam;
    memset(&stRecvParam, 0, sizeof(VENC_RECV_PIC_PARAM_S));
    stRecvParam.s32RecvPicNum = -1;
    ret = RK_MPI_VENC_StartRecvFrame(VENC_CHANNEL, &stRecvParam);
    if (ret != RK_SUCCESS)
    {
        RK_LOGE("error RK_MPI_VENC_StartRecvFrame, %d", ret);
        goto out;
    }
    venc_running = true;

out:
    pthread_mutex_unlock(&venc_state_mutex);
    if (ret != RK_SUCCESS)
    {
        return ret;
    }

    venc_read_thread = malloc(sizeof(pthread_t));
    if (pthread_create(venc_read_thread, NULL, venc_read_stream, NULL) != 0)
    {
        RK_LOGE("Failed to create venc_read_thread");
        return RK_FAILURE;
    }

    return RK_SUCCESS;
}

// Applies p to the running channel without recreating it; caller holds
// venc_state_mutex with venc_running set. Bitrate/QP/AQ changes leave the GOP
// alone; a frame-rate or GOP-length change makes MPP (under rockit) resend
// SPS/PPS with an IDR (mpp_enc_proc_rc_cfg -> enc_set_resend_hdr). Returns
// false on any failure so the caller can fall back to a stream restart.
static bool venc_apply_live_locked(const jetkvm_venc_params_t *p)
{
    const RK_U32 encode_fps = venc_encode_fps(venc_live.source_fps, p->max_fps);
    const RK_U32 gop = venc_gop_frames(encode_fps, p->gop_seconds);
    RK_U32 bitrate, max_bitrate;
    venc_bitrates(p, venc_live.width, venc_live.height, &bitrate, &max_bitrate);

    if (bitrate != venc_live.bitrate || max_bitrate != venc_live.max_bitrate ||
        encode_fps != venc_live.encode_fps || gop != venc_live.gop)
    {
        VENC_CHN_ATTR_S stAttr;
        memset(&stAttr, 0, sizeof(stAttr));
        int32_t ret = RK_MPI_VENC_GetChnAttr(VENC_CHANNEL, &stAttr);
        if (ret == RK_SUCCESS)
        {
            venc_set_rc_attr(&stAttr, venc_live.hevc, bitrate, max_bitrate, encode_fps, gop + venc_live.gop_nudge);
            ret = RK_MPI_VENC_SetChnAttr(VENC_CHANNEL, &stAttr);
        }
        if (ret != RK_SUCCESS)
        {
            log_error("live VENC rate update failed: %#x", ret);
            return false;
        }
        venc_live.bitrate = bitrate;
        venc_live.max_bitrate = max_bitrate;
        venc_live.gop = gop;
        venc_live.encode_fps = encode_fps;
    }

    if (p->min_qp != venc_live.params.min_qp || p->max_qp != venc_live.params.max_qp ||
        p->aq != venc_live.params.aq || p->still_qp != venc_live.params.still_qp)
    {
        if (!venc_configure_rc_for_desktop(p, venc_live.hevc))
        {
            return false;
        }
    }

    venc_live.params = *p;
    log_notice("applied VENC params live: gop %u frames (%.1f s at %u of %u fps), bitrate %u kbps (max %u)",
               gop, p->gop_seconds, encode_fps, venc_live.source_fps, bitrate, max_bitrate);
    return true;
}

static int32_t venc_stop()
{
    venc_set_running(false);

    int32_t ret;
    ret = RK_MPI_VENC_StopRecvFrame(VENC_CHANNEL);
    if (ret != RK_SUCCESS)
    {
        RK_LOGE("Failed to stop receiving frames for VENC_CHANNEL, error code: %d", ret);
        return ret;
    }

    if (venc_read_thread != NULL)
    {
        pthread_join(*venc_read_thread, NULL);
        free(venc_read_thread);
        venc_read_thread = NULL;
    }

    ret = RK_MPI_VENC_DestroyChn(VENC_CHANNEL);
    if (ret != RK_SUCCESS)
    {
        RK_LOGE("Failed to destroy VENC_CHANNEL, error code: %d", ret);
        return ret;
    }

    return RK_SUCCESS;
}

// Encoder output counters, bumped by venc_read_stream for every frame / IDR;
// video_request_keyframe waits on them to verify that a request worked.
static pthread_mutex_t venc_out_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t venc_out_cond = PTHREAD_COND_INITIALIZER;
static uint64_t venc_out_frames = 0;
static uint64_t venc_out_idrs = 0;

// True if the first VCL NAL unit of an Annex-B access unit is an IDR slice.
// Parsed from the bitstream because rockit's pack DataType is not reliable.
static bool annexb_is_idr(const uint8_t *data, size_t len, bool hevc)
{
    const size_t limit = len < 4096 ? len : 4096;
    for (size_t i = 0; i + 3 < limit; i++)
    {
        if (data[i] != 0 || data[i + 1] != 0 || data[i + 2] != 1)
        {
            continue;
        }
        const uint8_t header = data[i + 3];
        if (hevc)
        {
            const uint8_t type = (header >> 1) & 0x3f;
            if (type <= 31) // VCL
            {
                return type == 19 || type == 20; // IDR_W_RADL, IDR_N_LP
            }
        }
        else
        {
            const uint8_t type = header & 0x1f;
            if (type >= 1 && type <= 5) // coded slice
            {
                return type == 5;
            }
        }
        i += 3;
    }
    return false;
}

static bool venc_count_output(const uint8_t *data, size_t len)
{
    const bool idr = annexb_is_idr(data, len, venc_live.hevc);
    pthread_mutex_lock(&venc_out_mutex);
    venc_out_frames++;
    if (idr)
    {
        venc_out_idrs++;
    }
    pthread_cond_broadcast(&venc_out_cond);
    pthread_mutex_unlock(&venc_out_mutex);
    if (idr)
    {
        log_info("VENC IDR frame: %u bytes", (unsigned)len);
    }
    return idr;
}

// MPP forces an IDR and resends SPS/PPS when the GOP length changes
// (mpp_enc_proc_rc_cfg -> enc_set_resend_hdr), so alternate it by one frame.
// Caller holds venc_state_mutex with venc_running set.
static int32_t venc_gop_nudge_locked(void)
{
    VENC_CHN_ATTR_S stAttr;
    memset(&stAttr, 0, sizeof(stAttr));
    int32_t ret = RK_MPI_VENC_GetChnAttr(VENC_CHANNEL, &stAttr);
    if (ret != RK_SUCCESS)
    {
        return ret;
    }
    const RK_U32 nudge = venc_live.gop_nudge ^ 1;
    venc_set_rc_attr(&stAttr, venc_live.hevc, venc_live.bitrate, venc_live.max_bitrate,
                     venc_live.encode_fps, venc_live.gop + nudge);
    ret = RK_MPI_VENC_SetChnAttr(VENC_CHANNEL, &stAttr);
    if (ret == RK_SUCCESS)
    {
        venc_live.gop_nudge = nudge;
    }
    return ret;
}

// Keyframe paths in fallback order. This rockit build rejects instant
// RequestIDR with RK_ERR_VENC_NOT_SUPPORT; the non-instant variant returns 0
// even when the encoder ignores it, so every path is verified by watching the
// encoder output for an IDR.
enum
{
    KF_REQUEST_IDR_INSTANT,
    KF_REQUEST_IDR,
    KF_GOP_NUDGE,
    KF_PATH_COUNT
};
static const char *const kf_path_names[KF_PATH_COUNT] = {
    "RK_MPI_VENC_RequestIDR(instant)",
    "RK_MPI_VENC_RequestIDR(non-instant)",
    "RK_MPI_VENC_SetChnAttr GOP nudge",
};
#define KF_VERIFY_FRAMES 6 // encoded frames without an IDR before a path counts as failed
#define KF_NOT_RUNNING 1

static pthread_mutex_t kf_mutex = PTHREAD_MUTEX_INITIALIZER;
// First path to try; the last one that worked. Atomic: the still refresh
// (encoder output thread) reads it.
static atomic_int kf_path = KF_REQUEST_IDR_INSTANT;
static int kf_path_logged = -1;

static int32_t kf_issue(int path, RK_U32 *encode_fps)
{
    int32_t ret = KF_NOT_RUNNING;
    pthread_mutex_lock(&venc_state_mutex);
    // Not running: nothing to do, a (re)created channel starts with an IDR.
    if (venc_running)
    {
        *encode_fps = venc_live.encode_fps;
        switch (path)
        {
        case KF_REQUEST_IDR_INSTANT:
            ret = RK_MPI_VENC_RequestIDR(VENC_CHANNEL, RK_TRUE);
            break;
        case KF_REQUEST_IDR:
            ret = RK_MPI_VENC_RequestIDR(VENC_CHANNEL, RK_FALSE);
            break;
        default:
            ret = venc_gop_nudge_locked();
            break;
        }
    }
    pthread_mutex_unlock(&venc_state_mutex);
    return ret;
}

typedef enum
{
    KF_SEEN,         // an IDR came out
    KF_MISSING,      // KF_VERIFY_FRAMES frames came out, none an IDR
    KF_INCONCLUSIVE, // too few frames (no signal, restart): don't judge the path
} kf_result_t;

static kf_result_t kf_wait_for_idr(uint64_t frames0, uint64_t idrs0, RK_U32 encode_fps)
{
    // Room for KF_VERIFY_FRAMES + 2 frame intervals plus encoder latency.
    const long timeout_ms = 200 + (KF_VERIFY_FRAMES + 2) * 1000L / (encode_fps > 0 ? encode_fps : 1);
    struct timespec deadline;
    clock_gettime(CLOCK_REALTIME, &deadline);
    deadline.tv_sec += timeout_ms / 1000;
    deadline.tv_nsec += (timeout_ms % 1000) * 1000000L;
    if (deadline.tv_nsec >= 1000000000L)
    {
        deadline.tv_sec++;
        deadline.tv_nsec -= 1000000000L;
    }

    kf_result_t result = KF_INCONCLUSIVE;
    pthread_mutex_lock(&venc_out_mutex);
    for (;;)
    {
        if (venc_out_idrs != idrs0)
        {
            result = KF_SEEN;
            break;
        }
        if (venc_out_frames - frames0 >= KF_VERIFY_FRAMES)
        {
            result = KF_MISSING;
            break;
        }
        if (pthread_cond_timedwait(&venc_out_cond, &venc_out_mutex, &deadline) == ETIMEDOUT)
        {
            break;
        }
    }
    pthread_mutex_unlock(&venc_out_mutex);
    return result;
}

int video_request_keyframe()
{
    int32_t status = -1;
    pthread_mutex_lock(&kf_mutex);
    for (int path = atomic_load(&kf_path); path < KF_PATH_COUNT; path++)
    {
        pthread_mutex_lock(&venc_out_mutex);
        const uint64_t frames0 = venc_out_frames;
        const uint64_t idrs0 = venc_out_idrs;
        pthread_mutex_unlock(&venc_out_mutex);

        RK_U32 encode_fps = 0;
        const int32_t ret = kf_issue(path, &encode_fps);
        if (ret == KF_NOT_RUNNING)
        {
            status = 0;
            break;
        }
        if (ret != RK_SUCCESS)
        {
            log_notice("keyframe path %s failed: %#x", kf_path_names[path], ret);
            continue;
        }

        const kf_result_t result = kf_wait_for_idr(frames0, idrs0, encode_fps);
        if (result == KF_MISSING)
        {
            log_notice("keyframe path %s produced no IDR within %d frames",
                       kf_path_names[path], KF_VERIFY_FRAMES);
            continue;
        }
        // Seen, or nothing to judge by (no frames flowing): keep this path.
        atomic_store(&kf_path, path);
        if (result == KF_SEEN && kf_path_logged != path)
        {
            log_notice("keyframe requests use %s", kf_path_names[path]);
            kf_path_logged = path;
        }
        status = 0;
        break;
    }
    if (status != 0)
    {
        log_error("all keyframe paths failed; next request starts over");
        atomic_store(&kf_path, KF_REQUEST_IDR_INSTANT);
    }
    pthread_mutex_unlock(&kf_mutex);
    return status;
}

// The still refresh's keyframe, by the path video_request_keyframe found to
// work (not verified here: this is the thread that would see the IDR).
static void still_request_keyframe(void)
{
    RK_U32 encode_fps = 0;
    kf_issue(atomic_load(&kf_path), &encode_fps);
}

struct buffer
{
    struct v4l2_plane plane_buffer;
    MB_BLK mb_blk;
};

const int input_buffer_count = 3;

static int32_t buf_init()
{
    MB_POOL_CONFIG_S stMbPoolCfg;
    memset(&stMbPoolCfg, 0, sizeof(MB_POOL_CONFIG_S));
    stMbPoolCfg.u64MBSize = 1920 * 1080 * 3; // max resolution
    stMbPoolCfg.u32MBCnt = input_buffer_count;
    stMbPoolCfg.enAllocType = MB_ALLOC_TYPE_DMA;
    stMbPoolCfg.bPreAlloc = RK_TRUE;
    memPool = RK_MPI_MB_CreatePool(&stMbPoolCfg);
    if (memPool == MB_INVALID_POOLID)
    {
        return -1;
    }
    log_info("created memory pool");

    return RK_SUCCESS;
}

pthread_t *format_thread = NULL;

int video_init(float factor)
{
    detect_sleep_mode();
    venc_params_load_env();

    if (factor <= 0) {
        factor = 1.0f;
    }
    quality_factor = factor;

    if (RK_MPI_SYS_Init() != RK_SUCCESS)
    {
        log_error("RK_MPI_SYS_Init failed");
        return RK_FAILURE;
    }

    if (sub_dev_fd < 0)
    {
        sub_dev_fd = open(SUB_DEV, O_RDWR);
        if (sub_dev_fd < 0)
        {
            log_error("failed to open control sub device %s: %s", SUB_DEV, strerror(errno));
            return errno;
        }
        log_info("opened control sub device %s", SUB_DEV);
    }

    int32_t ret = buf_init();
    if (ret != RK_SUCCESS)
    {
        log_error("buf_init failed with error: %d", ret);
        return ret;
    }
    log_info("buf_init completed successfully");

    format_thread = malloc(sizeof(pthread_t));
    pthread_create(format_thread, NULL, run_detect_format, NULL);
    return RK_SUCCESS;
}

// static int32_t venc_set_param(int32_t bitrate, int32_t max_bitrate, int32_t width, int32_t height)
// {

//     VENC_CHN_ATTR_S stAttr;
//     populate_venc_attr(&stAttr, bitrate, max_bitrate, width, height);
//     VENC_CHN_PARAM_S stParam;
//     memset(&stParam, 0, sizeof(VENC_CHN_PARAM_S));

//     RK_MPI_VENC_StopRecvFrame(VENC_CHANNEL);

//     int32_t ret = RK_MPI_VENC_SetChnParam(VENC_CHANNEL, &stAttr);
//     if (ret < 0)
//     {
//         RK_LOGE("error RK_MPI_VENC_SetChnParam, %d", ret);
//         return ret;
//     }
//     VENC_RECV_PIC_PARAM_S stRecvParam;
//     memset(&stRecvParam, 0, sizeof(VENC_RECV_PIC_PARAM_S));
//     stRecvParam.s32RecvPicNum = -1;
//     ret = RK_MPI_VENC_StartRecvFrame(VENC_CHANNEL, &stRecvParam);
//     if (ret < 0)
//     {
//         RK_LOGE("error RK_MPI_VENC_StartRecvFrame, %d", ret);
//         return ret;
//     }

//     return RK_SUCCESS;
// }

/**
 * @brief Continuously reads encoded video streams and sends them over unix socket.
 *
 * @param arg Unused parameter (void pointer for thread compatibility)
 * @return NULL Always returns NULL
 */
static void *venc_read_stream(void *arg)
{
    (void)arg;
    void *pData = RK_NULL;
    int loopCount = 0;
    int s32Ret;

    VENC_STREAM_S stFrame;
    stFrame.pstPack = malloc(sizeof(VENC_PACK_S));
    while (venc_running)
    {
        log_trace("RK_MPI_VENC_GetStream");
        s32Ret = RK_MPI_VENC_GetStream(VENC_CHANNEL, &stFrame, 200); // blocks max 200ms
        if (s32Ret == RK_SUCCESS)
        {
            RK_U64 nowUs = get_us();
            log_trace("chn:0, loopCount:%d enc->seq:%d wd:%d pts=%llu delay=%lldus",
                   loopCount, stFrame.u32Seq, stFrame.pstPack->u32Len,
                   stFrame.pstPack->u64PTS, nowUs - stFrame.pstPack->u64PTS);
            pData = RK_MPI_MB_Handle2VirAddr(stFrame.pstPack->pMbBlk);
            const size_t len = stFrame.pstPack->u32Len;
            const bool idr = venc_count_output(pData, len);
            video_send_frame(pData, (ssize_t)len);
            s32Ret = RK_MPI_VENC_ReleaseStream(VENC_CHANNEL, &stFrame);
            if (s32Ret != RK_SUCCESS)
            {
                log_error("RK_MPI_VENC_ReleaseStream fail %x", s32Ret);
            }
            still_on_frame(len, idr);
            loopCount++;
        }
        else
        {
            if (s32Ret == RK_ERR_VENC_BUF_EMPTY)
            {
                continue;
            }
            log_error("RK_MPI_VENC_GetStream fail %x", s32Ret);
            break;
        }
    }
    log_info("exiting venc_read_stream");
    free(stFrame.pstPack);
    return NULL;
}

uint32_t detected_width, detected_height;
// detected_fps is the rounded source vrefresh from the latest dv-timings query.
// 60 is a safe default until the first SOURCE_CHANGE event fires.
uint32_t detected_fps = 60;
bool detected_signal = false, streaming_flag = false;

bool streaming_stopped = true;
pthread_mutex_t streaming_stopped_mutex = PTHREAD_MUTEX_INITIALIZER;

pthread_t *streaming_thread = NULL;
pthread_mutex_t streaming_mutex = PTHREAD_MUTEX_INITIALIZER;

bool get_streaming_flag()
{
    log_info("getting streaming flag");
    pthread_mutex_lock(&streaming_mutex);
    bool flag = streaming_flag;
    pthread_mutex_unlock(&streaming_mutex);
    return flag;
}

void set_streaming_flag(bool flag)
{
    log_info("setting streaming flag to %d", flag);

    pthread_mutex_lock(&streaming_mutex);
    streaming_flag = flag;
    pthread_mutex_unlock(&streaming_mutex);

    video_send_format_report();
}

void set_streaming_stopped(bool stopped)
{
    pthread_mutex_lock(&streaming_stopped_mutex);
    streaming_stopped = stopped;
    pthread_mutex_unlock(&streaming_stopped_mutex);

    video_send_format_report();
}

bool get_streaming_stopped()
{
    pthread_mutex_lock(&streaming_stopped_mutex);
    bool stopped = streaming_stopped;
    pthread_mutex_unlock(&streaming_stopped_mutex);
    return stopped;
}

void write_buffer_to_file(const uint8_t *buffer, size_t length, const char *filename)
{
    FILE *file = fopen(filename, "wb");
    fwrite(buffer, 1, length, file);
    fclose(file);
}

void *run_video_stream(void *arg)
{
    enum v4l2_buf_type type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;

    log_info("running video stream");

    set_streaming_stopped(false);
    while (streaming_flag)
    {
        if (detected_signal == false)
        {
            usleep(10000); // Reduced to 10ms for better responsiveness to streaming_flag changes
            continue;
        }

        int video_dev_fd = open(VIDEO_DEV, O_RDWR);
        if (video_dev_fd < 0)
        {
            log_error("failed to open video capture device %s: %s", VIDEO_DEV, strerror(errno));
            usleep(1000000);
            continue;
        }
        log_info("opened video capture device %s", VIDEO_DEV);

        uint32_t width = detected_width;
        uint32_t height = detected_height;
        uint32_t fps = detected_fps;
        struct v4l2_format fmt;
        memset(&fmt, 0, sizeof(struct v4l2_format));
        fmt.type = type;
        fmt.fmt.pix_mp.width = width;
        fmt.fmt.pix_mp.height = height;
        fmt.fmt.pix_mp.pixelformat = V4L2_PIX_FMT_YUYV;
        fmt.fmt.pix_mp.field = V4L2_FIELD_ANY;
        set_v4l2_yuv_colorimetry(&fmt.fmt.pix_mp);

        if (ioctl(video_dev_fd, VIDIOC_S_FMT, &fmt) < 0)
        {
            log_error("Set format fail: %s", strerror(errno));
            usleep(100000); // Sleep for 100 milliseconds
            close(video_dev_fd);
            continue;
        }
        log_info("capture colorimetry: colorspace=%u ycbcr_enc=%u quantization=%u xfer=%u",
                 fmt.fmt.pix_mp.colorspace, fmt.fmt.pix_mp.ycbcr_enc,
                 fmt.fmt.pix_mp.quantization, fmt.fmt.pix_mp.xfer_func);

        struct v4l2_buffer buf;

        struct v4l2_requestbuffers req;
        req.count = input_buffer_count;
        req.type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;
        req.memory = V4L2_MEMORY_DMABUF;

        if (ioctl(video_dev_fd, VIDIOC_REQBUFS, &req) < 0)
        {
            log_error("VIDIOC_REQBUFS failed: %s", strerror(errno));
            close(video_dev_fd);
            return (void *)errno;
        }
        log_info("VIDIOC_REQBUFS successful");

        struct buffer buffers[3] = {};
        log_info("allocated buffers");

        for (int i = 0; i < input_buffer_count; i++)
        {
            struct v4l2_plane *planes_buffer = &buffers[i].plane_buffer;
            memset(planes_buffer, 0, sizeof(struct v4l2_plane));

            memset(&buf, 0, sizeof(buf));
            buf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;
            buf.memory = V4L2_MEMORY_DMABUF;
            buf.m.planes = planes_buffer;
            buf.length = 1;
            buf.index = i;

            if (-1 == ioctl(video_dev_fd, VIDIOC_QUERYBUF, &buf))
            {
                log_error("VIDIOC_QUERYBUF failed: %s", strerror(errno));
                req.count = i;
                close(video_dev_fd);
                return (void *)errno;
            }
            log_info("VIDIOC_QUERYBUF successful for buffer %d", i);

            log_info("plane: length = %d", planes_buffer->length);
            log_info("plane: offset = %d", planes_buffer->m.mem_offset);

            MB_BLK blk = RK_MPI_MB_GetMB(memPool, (planes_buffer)->length, RK_TRUE);
            if (blk == NULL)
            {
                log_error("get mb blk failed!");
                close(video_dev_fd);
                return (void *)errno;
            }
            log_info("Got memory block for buffer %d", i);

            buffers[i].mb_blk = blk;

            RK_S32 buf_fd = (RK_MPI_MB_Handle2Fd(blk));
            if (buf_fd < 0)
            {
                log_error("RK_MPI_MB_Handle2Fd failed!");
                close(video_dev_fd);
                return (void *)errno;
            }
            log_info("Converted memory block to file descriptor for buffer %d", i);
            planes_buffer->m.fd = buf_fd;
        }

        for (int i = 0; i < input_buffer_count; ++i)
        {
            struct v4l2_buffer buf;
            memset(&buf, 0, sizeof(buf));
            buf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;
            buf.memory = V4L2_MEMORY_DMABUF;
            buf.length = 1;
            buf.index = i;
            buf.m.planes = &buffers[i].plane_buffer;
            if (ioctl(video_dev_fd, VIDIOC_QBUF, &buf) < 0)
            {
                log_error("VIDIOC_QBUF failed: %s", strerror(errno));
                close(video_dev_fd);
                return (void *)errno;
            }
            log_info("VIDIOC_QBUF successful for buffer %d", i);
        }

        if (ioctl(video_dev_fd, VIDIOC_STREAMON, &type) < 0)
        {
            log_error("VIDIOC_STREAMON failed: %s", strerror(errno));
            close(video_dev_fd);
            return (void *)errno;
        }

        struct v4l2_plane tmp_plane;

        // Set VENC parameters
        RK_S32 ret = venc_start(width, height, fps);
        if (ret != RK_SUCCESS)
        {
            log_error("Set VENC parameters failed with %#x", ret);
            goto cleanup;
        }

        fd_set fds;
        struct timeval tv;
        int r;
        uint32_t num = 0;
        uint32_t frame_credit = 0;
        VIDEO_FRAME_INFO_S stFrame;
        memset(&stFrame, 0, sizeof(VIDEO_FRAME_INFO_S));
        stFrame.stVFrame.u32Width = width;
        stFrame.stVFrame.u32Height = height;
        stFrame.stVFrame.u32VirWidth = RK_ALIGN_16(width);
        stFrame.stVFrame.u32VirHeight = RK_ALIGN_16(height);
        stFrame.stVFrame.enField = VIDEO_FIELD_FRAME;
        stFrame.stVFrame.enPixelFormat = RK_FMT_YUV422_YUYV;
        stFrame.stVFrame.enVideoFormat = VIDEO_FORMAT_LINEAR;
        stFrame.stVFrame.enCompressMode = COMPRESS_MODE_NONE;
        stFrame.stVFrame.enDynamicRange = DYNAMIC_RANGE_SDR8;
        stFrame.stVFrame.enColorGamut = COLOR_GAMUT_BT601;

        while (streaming_flag)
        {
            FD_ZERO(&fds);
            FD_SET(video_dev_fd, &fds);
            tv.tv_sec = 1;
            tv.tv_usec = 0;

            r = select(video_dev_fd + 1, &fds, NULL, NULL, &tv);
            if (r == 0)
            {
                log_info("select timeout");
                ensure_sleep_mode_disabled();
                break;
            }
            if (r == -1)
            {
                if (errno == EINTR)
                {
                    continue;
                }
                log_error("select in video streaming");
                break;
            }
            memset(&buf, 0, sizeof(buf));
            buf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;
            buf.memory = V4L2_MEMORY_DMABUF;
            buf.m.planes = &tmp_plane;
            buf.length = 1;
            if (ioctl(video_dev_fd, VIDIOC_DQBUF, &buf) < 0)
            {
                log_error("VIDIOC_DQBUF failed: %s", strerror(errno));
                break;
            }
            log_trace("got frame, bytesused = %d", tmp_plane.bytesused);
            // Drop frames evenly to encode encode_fps of fps. Done here rather
            // than via the VENC dst frame rate, whose drop behaviour rockit
            // does not document; the encoder is configured for encode_fps.
            // encode_fps can change live (venc_apply_live_locked).
            frame_credit += venc_live.encode_fps;
            if (frame_credit < fps)
            {
                if (ioctl(video_dev_fd, VIDIOC_QBUF, &buf) < 0)
                    log_error("failure VIDIOC_QBUF: %s", strerror(errno));
                continue;
            }
            frame_credit -= fps;
            MB_BLK blk = RK_MPI_MMZ_Fd2Handle(tmp_plane.m.fd);
            assert(blk != RK_NULL);
            stFrame.stVFrame.pMbBlk = blk;
            stFrame.stVFrame.u32TimeRef = num;
            stFrame.stVFrame.u64PTS = get_us();
            bool retried = false;
        retry_send_frame:
            if (RK_MPI_VENC_SendFrame(VENC_CHANNEL, &stFrame, 2000) != RK_SUCCESS)
            {
                if (retried == true)
                {
                    log_error("RK_MPI_VENC_SendFrame retry failed");
                }
                else
                {
                    log_error("RK_MPI_VENC_SendFrame failed,retrying");
                    retried = true;
                    usleep(1000llu);
                    goto retry_send_frame;
                }
            }

            num++;

            if (ioctl(video_dev_fd, VIDIOC_QBUF, &buf) < 0)
                log_error("failure VIDIOC_QBUF: %s", strerror(errno));
        }
    cleanup:
        log_info("cleaning up video capture device %s", VIDEO_DEV);
        if (ioctl(video_dev_fd, VIDIOC_STREAMOFF, &type) < 0)
        {
            log_error("VIDIOC_STREAMOFF failed: %s", strerror(errno));
        }

        // Explicitly free V4L2 buffer queue
        struct v4l2_requestbuffers req_free;
        memset(&req_free, 0, sizeof(req_free));
        req_free.count = 0;
        req_free.type = V4L2_BUF_TYPE_VIDEO_CAPTURE_MPLANE;
        req_free.memory = V4L2_MEMORY_DMABUF;

        if (ioctl(video_dev_fd, VIDIOC_REQBUFS, &req_free) < 0)
        {
            log_error("Failed to free V4L2 buffers: %s", strerror(errno));
        }

        venc_stop();

        for (int i = 0; i < input_buffer_count; i++)
        {
            if (buffers[i].mb_blk != NULL)
            {
                RK_MPI_MB_ReleaseMB((buffers + i)->mb_blk);
            }
        }

        log_info("closing video capture device %s", VIDEO_DEV);
        close(video_dev_fd);
    }

    log_info("video stream thread exiting");

    set_streaming_stopped(true);

    return NULL;
}

void video_shutdown()
{
    if (should_exit == true)
    {
        log_info("shutting down in progress already");
        return;
    }
    video_stop_streaming();

    should_exit = true;
    if (sub_dev_fd > 0)
    {
        shutdown(sub_dev_fd, SHUT_RDWR);
        log_info("Closed sub_dev_fd");
    }

    if (memPool != MB_INVALID_POOLID)
    {
        RK_MPI_MB_DestroyPool(memPool);
    }
    log_info("Destroyed memory pool");

    pthread_mutex_destroy(&streaming_mutex);
    log_info("Destroyed streaming mutex");
}

void video_start_streaming()
{
    log_info("starting video streaming");
    if (streaming_thread != NULL)
    {
        bool stopped = get_streaming_stopped();
        if (stopped == true) {
            log_error("video streaming already stopped but streaming_thread is not NULL");
            assert(stopped == true);
        }
        log_warn("video streaming already started");
        return;
    }

    pthread_t *new_thread = malloc(sizeof(pthread_t));
    if (new_thread == NULL)
    {
        log_error("Failed to allocate memory for streaming thread");
        return;
    }

    set_streaming_flag(true);
    int result = pthread_create(new_thread, NULL, run_video_stream, NULL);
    if (result != 0)
    {
        log_error("Failed to create streaming thread: %s", strerror(result));
        set_streaming_flag(false);
        free(new_thread);
        return;
    }

    // Only set streaming_thread after successful creation
    streaming_thread = new_thread;
}

bool wait_for_streaming_stopped()
{
    int attempts = 0;
    while (attempts < 30) {
        if (get_streaming_stopped() == true) {
            log_info("video streaming stopped after %d attempts", attempts);
            return true;
        }
        usleep(100000); // 100ms
        attempts++;
    }
    log_error("video streaming did not stop after 3s");
    return false;
}

void video_stop_streaming()
{
    if (streaming_thread == NULL) {
        log_info("video streaming already stopped");
        return;
    }

    log_info("stopping video streaming");
    set_streaming_flag(false);

    log_info("waiting for video streaming thread to exit");
    wait_for_streaming_stopped();

    pthread_join(*streaming_thread, NULL);
    free(streaming_thread);
    streaming_thread = NULL;

    log_info("video streaming stopped");
}

uint8_t video_get_streaming_status() {
    // streaming flag can be false when stopping streaming
    if (get_streaming_flag() == true) return 1;
    if (get_streaming_stopped() == false) return 2;
    return 0;
}

void video_restart_streaming()
{
    uint8_t streaming_status = video_get_streaming_status();
    // 0 = stopped, 1 = running, 2 = stopping

    if (streaming_status == 0)
    {
        log_info("will not restart video streaming because it's stopped");
        return;
    }

    if (streaming_status == 1 || streaming_status == 2) {
        video_stop_streaming();
    }

    if (!wait_for_streaming_stopped()) {
        return;
    }

    video_start_streaming();
}

void *run_detect_format(void *arg)
{
    struct v4l2_event_subscription sub;
    struct v4l2_event ev;
    struct v4l2_dv_timings dv_timings;

    memset(&sub, 0, sizeof(sub));
    sub.type = V4L2_EVENT_SOURCE_CHANGE;
    if (ioctl(sub_dev_fd, VIDIOC_SUBSCRIBE_EVENT, &sub) == -1)
    {
        log_error("cannot subscribe to event");
        goto exit;
    }

    while (!should_exit)
    {
        ensure_sleep_mode_disabled();

        memset(&dv_timings, 0, sizeof(dv_timings));
        if (ioctl(sub_dev_fd, VIDIOC_QUERY_DV_TIMINGS, &dv_timings) != 0)
        {
            detected_signal = false;
            if (errno == ENOLINK)
            {
                // No timings could be detected because no signal was found.
                log_info("HDMI status: no signal");
                video_report_format(false, "no_signal", 0, 0, 0);
            }
            else if (errno == ENOLCK)
            {
                // The signal was unstable and the hardware could not lock on to it.
                log_info("HDMI status: no lock");
                video_report_format(false, "no_lock", 0, 0, 0);
            }
            else if (errno == ERANGE)
            {
                // Timings were found, but they are out of range of the hardware capabilities.
                log_warn("HDMI status: out of range");
                video_report_format(false, "out_of_range", 0, 0, 0);
            }
            else
            {
                log_error("error VIDIOC_QUERY_DV_TIMINGS: %s", strerror(errno));
                sleep(1);
                continue;
            }
        }
        else
        {
            log_info("Active width: %d", dv_timings.bt.width);
            log_info("Active height: %d", dv_timings.bt.height);
            double frames_per_second = (double)dv_timings.bt.pixelclock /
                                       ((dv_timings.bt.height + dv_timings.bt.vfrontporch + dv_timings.bt.vsync +
                                         dv_timings.bt.vbackporch) *
                                        (dv_timings.bt.width + dv_timings.bt.hfrontporch + dv_timings.bt.hsync +
                                         dv_timings.bt.hbackporch));
            log_info("Frames per second: %.2f fps", frames_per_second);

            // Round to nearest integer for the encoder rate control. CVT-RB
            // sources land a touch under (e.g. 119.91); rounding keeps the
            // encoder sized for the nominal rate.
            uint32_t fps_int = (uint32_t)(frames_per_second + 0.5);
            if (fps_int < 1) fps_int = 1;
            // Tolerate ±1 fps wobble between SOURCE_CHANGE events without
            // tearing down the pipeline.
            bool fps_changed = fps_int > detected_fps + 1 || fps_int + 1 < detected_fps;
            bool should_restart = dv_timings.bt.width != detected_width || dv_timings.bt.height != detected_height || fps_changed || !detected_signal;

            detected_width = dv_timings.bt.width;
            detected_height = dv_timings.bt.height;
            detected_fps = fps_int;
            detected_signal = true;
            video_report_format(true, NULL, detected_width, detected_height, frames_per_second);

            if (should_restart) {
                log_info("restarting video streaming due to format change");
                video_restart_streaming();
            }
        }

        memset(&ev, 0, sizeof(ev));
        if (ioctl(sub_dev_fd, VIDIOC_DQEVENT, &ev) != 0)
        {
            log_error("failed to VIDIOC_DQEVENT: %s", strerror(errno));
            break;
        }
        log_info("New event of type %u", ev.type);
        if (ev.type != V4L2_EVENT_SOURCE_CHANGE)
        {
            continue;
        }
        log_info("source change detected!");
    }
exit:
    close(sub_dev_fd);
    return NULL;
}


// Drops runtime overrides (video_set_encoder_params). Applies at the next
// channel start; callers stop or restart the stream right after.
void video_reset_encoder_params(void)
{
    pthread_mutex_lock(&venc_params_mutex);
    const jetkvm_venc_params_t *d = &venc_params_defaults;
    const jetkvm_venc_params_t *p = &venc_params;
    const bool changed = p->min_qp != d->min_qp || p->max_qp != d->max_qp ||
                         p->gop_seconds != d->gop_seconds || p->aq != d->aq ||
                         p->max_fps != d->max_fps || p->bitrate_kbps != d->bitrate_kbps ||
                         p->max_bitrate_kbps != d->max_bitrate_kbps || p->still_qp != d->still_qp;
    venc_params = venc_params_defaults;
    pthread_mutex_unlock(&venc_params_mutex);
    if (changed)
    {
        log_notice("encoder params reset to defaults");
    }
}

void video_set_quality_factor(float factor)
{
    quality_factor = factor;

    // A quality change from the UI should take full effect: drop any runtime
    // overrides a client's quality governor left behind.
    video_reset_encoder_params();

    // TODO: update venc bitrate without stopping streaming
    video_restart_streaming();
}

float video_get_quality_factor() {
    return quality_factor;
}

int video_set_encoder_params(const jetkvm_venc_params_t *params)
{
    if (params == NULL || !venc_params_valid(params))
    {
        return -1;
    }

    pthread_mutex_lock(&venc_state_mutex);
    pthread_mutex_lock(&venc_params_mutex);
    venc_params = *params;
    pthread_mutex_unlock(&venc_params_mutex);
    log_notice("encoder params set: qp %d-%d (still %d), gop %.1f s, aq %s, max fps %d, bitrate %d/%d kbps (0 = auto)",
               params->min_qp, params->max_qp, params->still_qp, params->gop_seconds,
               params->aq ? "rockchip default" : "off", params->max_fps,
               params->bitrate_kbps, params->max_bitrate_kbps);
    // No running channel: the next venc_start() picks the values up.
    const bool applied = !venc_running || venc_apply_live_locked(params);
    pthread_mutex_unlock(&venc_state_mutex);

    if (!applied)
    {
        // Same path as video_set_quality_factor: restarts capture + VENC
        // only; EDID and HDMI hot-plug are untouched.
        log_notice("live VENC update failed, restarting the stream");
        video_restart_streaming();
    }
    return 0;
}

void video_get_encoder_status(jetkvm_venc_status_t *status)
{
    memset(status, 0, sizeof(*status));
    status->params = venc_get_params();
    status->source_fps = detected_fps;
    status->encode_fps = venc_encode_fps(detected_fps, status->params.max_fps);
    status->gop_frames = venc_gop_frames(status->encode_fps, status->params.gop_seconds);
    RK_U32 bitrate, max_bitrate;
    venc_bitrates(&status->params, detected_width, detected_height, &bitrate, &max_bitrate);
    status->effective_bitrate_kbps = bitrate;
    status->effective_max_bitrate_kbps = max_bitrate;
    status->codec = codec_type;
    status->streaming = video_get_streaming_status() == 1;
}

void video_set_codec_type(int type) {
    codec_type = type;
}

int video_get_codec_type() {
    return codec_type;
}
