#define _POSIX_C_SOURCE 200809L
#include <unistd.h>
#include <time.h>
#include <rk_type.h>
#include <rk_mpi_venc.h>
#include <rk_mpi_sys.h>
#include <string.h>
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
    stAttr->stVencAttr.u32BufSize = width * height * 3 / 2;
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

    log_info("configured VENC VUI as limited-range, BT.601 matrix, BT.709 primaries/transfer");
}

pthread_t *venc_read_thread = NULL;
volatile bool venc_running = false;
// Guards venc_running and venc_live. Held across channel creation and every
// call that touches the running channel from another thread (IDR requests),
// so none of them races RK_MPI_VENC_DestroyChn.
static pthread_mutex_t venc_state_mutex = PTHREAD_MUTEX_INITIALIZER;

// What the running channel is configured with.
static struct
{
    bool hevc;
    RK_U32 encode_fps;
    RK_U32 gop;       // nominal GOP length
    RK_U32 gop_nudge; // 0/1 frame added by the keyframe GOP-nudge fallback
    RK_U32 bitrate;
    RK_U32 max_bitrate;
} venc_live;

static void venc_set_running(bool running)
{
    pthread_mutex_lock(&venc_state_mutex);
    venc_running = running;
    pthread_mutex_unlock(&venc_state_mutex);
}

static int32_t venc_start(int32_t bitrate, int32_t max_bitrate, int32_t width, int32_t height, int32_t fps)
{
    int32_t ret;
    VENC_CHN_ATTR_S stAttr;

    pthread_mutex_lock(&venc_state_mutex);
    venc_live.hevc = codec_type == 1;
    venc_live.encode_fps = fps;
    // GOP scales with framerate so IDR cadence stays ~0.5s regardless of source
    // refresh — keeps WebRTC recovery latency bounded at 60 Hz and 120 Hz alike.
    venc_live.gop = fps > 0 ? fps / 2 : 30;
    if (venc_live.gop < 1) venc_live.gop = 1;
    venc_live.gop_nudge = 0;
    venc_live.bitrate = bitrate;
    venc_live.max_bitrate = max_bitrate;

    populate_venc_attr(&stAttr, bitrate, max_bitrate, width, height, fps, venc_live.gop);
    ret = RK_MPI_VENC_CreateChn(VENC_CHANNEL, &stAttr);
    if (ret != RK_SUCCESS)
    {
        RK_LOGE("error RK_MPI_VENC_CreateChn, %d", ret);
        goto out;
    }

    venc_configure_limited_range_vui();

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

static void venc_count_output(const uint8_t *data, size_t len)
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
static int kf_path = KF_REQUEST_IDR_INSTANT; // first path to try; the last one that worked
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
    for (int path = kf_path; path < KF_PATH_COUNT; path++)
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
        kf_path = path;
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
        kf_path = KF_REQUEST_IDR_INSTANT;
    }
    pthread_mutex_unlock(&kf_mutex);
    return status;
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
            venc_count_output(pData, stFrame.pstPack->u32Len);
            video_send_frame(pData, (ssize_t)stFrame.pstPack->u32Len);
            s32Ret = RK_MPI_VENC_ReleaseStream(VENC_CHANNEL, &stFrame);
            if (s32Ret != RK_SUCCESS)
            {
                log_error("RK_MPI_VENC_ReleaseStream fail %x", s32Ret);
            }
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
        int32_t bitrate = calculate_bitrate(quality_factor, width, height);
        RK_S32 ret = venc_start(bitrate, bitrate * 3 / 2, width, height, fps);
        if (ret != RK_SUCCESS)
        {
            log_error("Set VENC parameters failed with %#x", ret);
            goto cleanup;
        }

        fd_set fds;
        struct timeval tv;
        int r;
        uint32_t num = 0;
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


void video_set_quality_factor(float factor)
{
    quality_factor = factor;

    // TODO: update venc bitrate without stopping streaming
    video_restart_streaming();
}

float video_get_quality_factor() {
    return quality_factor;
}

void video_set_codec_type(int type) {
    codec_type = type;
}

int video_get_codec_type() {
    return codec_type;
}
