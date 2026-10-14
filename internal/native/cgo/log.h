#ifndef VIDEO_DAEMON_LOG_H
#define VIDEO_DAEMON_LOG_H

#include <stdio.h>
#include <string.h>
#include <time.h>
#include "log_handler.h"

/* Default level */
#ifndef LOG_LEVEL
    #define LOG_LEVEL   LEVEL_INFO
#endif

#define __FILENAME__ (strrchr(__FILE__, '/') ? strrchr(__FILE__, '/') + 1 : __FILE__)

void jetkvm_log(const char *message);

/* Log to screen. A longer message is truncated, never written past the
 * buffer: it can carry text from the main app (D15). */
#define emit_log(level, file, func, line, ...) do {                              \
    /* call the log handler */                                                   \
    char msg_buffer[1024];                                                       \
    snprintf(msg_buffer, sizeof(msg_buffer), __VA_ARGS__);                       \
    log_message(level, file, func, line, msg_buffer);                            \
} while (0)

/* Level enum */
#define LEVEL_PANIC   5
#define LEVEL_FATAL   4
#define LEVEL_ERROR   3
#define LEVEL_WARN    2
#define LEVEL_INFO    1
#define LEVEL_DEBUG   0
#define LEVEL_TRACE   -1
/* zerolog.NoLevel: emitted at any configured log level (rendered as info) */
#define LEVEL_NOTICE  6

/* TRACE LOG */
#define log_trace(...) do {                                                        \
    if (LOG_LEVEL <= LEVEL_TRACE) {                                                \
        emit_log(                                                                  \
            LEVEL_TRACE, __FILENAME__, __func__, __LINE__, __VA_ARGS__             \
        );                                                                         \
    }                                                                              \
} while (0)

/* DEBUG LOG */
#define log_debug(...) do {                                                        \
    if (LOG_LEVEL <= LEVEL_DEBUG) {                                                \
        emit_log(                                                                  \
            LEVEL_DEBUG, __FILENAME__, __func__, __LINE__, __VA_ARGS__             \
        );                                                                         \
    }                                                                              \
} while (0)

/* INFO LOG */
#define log_info(...) do {                                                         \
    if (LOG_LEVEL <= LEVEL_INFO) {                                                 \
        emit_log(                                                                  \
            LEVEL_INFO, __FILENAME__, __func__, __LINE__, __VA_ARGS__              \
        );                                                                         \
    }                                                                              \
} while (0)

/* NOTICE LOG: rare, important state (e.g. encoder config); always emitted.
 * The native process logs at ERROR unless JETKVM_LOG_* env says otherwise. */
#define log_notice(...) do {                                                       \
    emit_log(                                                                      \
        LEVEL_NOTICE, __FILENAME__, __func__, __LINE__, __VA_ARGS__                \
    );                                                                             \
} while (0)

/* WARN LOG */
#define log_warn(...) do {                                                         \
    if (LOG_LEVEL <= LEVEL_WARN) {                                                 \
        emit_log(                                                                  \
            LEVEL_WARN, __FILENAME__, __func__, __LINE__, __VA_ARGS__              \
        );                                                                         \
    }                                                                              \
} while (0)

/* ERROR LOG */
#define log_error(...) do {                                                        \
    if (LOG_LEVEL <= LEVEL_ERROR) {                                                \
        emit_log(                                                                  \
            LEVEL_ERROR, __FILENAME__, __func__, __LINE__, __VA_ARGS__             \
        );                                                                         \
    }                                                                              \
} while (0)

/* PANIC LOG */
#define log_panic(...) do {                                                        \
    if (LOG_LEVEL <= LEVEL_PANIC) {                                                \
        emit_log(                                                                  \
            LEVEL_PANIC, __FILENAME__, __func__, __LINE__, __VA_ARGS__             \
        );                                                                         \
    }                                                                              \
} while (0)

#endif //VIDEO_DAEMON_LOG_H
