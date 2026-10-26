/*
 * disc_ui_hook.c - DISC UI Extension Wrapper & Menu Hook
 * Cross-compiled for MIPS32 little-endian (mipsel-linux-gnu, glibc 2.29, o32 ABI)
 */

typedef unsigned int size_t;
typedef int ssize_t;
typedef int pid_t;

#define RTLD_LAZY   0x0001
#define RTLD_NOW    0x0002
#define RTLD_GLOBAL 0x0100

/* MIPS Linux open() flags */
#define O_RDONLY    0x0000
#define O_WRONLY    0x0001
#define O_RDWR      0x0002
#define O_APPEND    0x0008
#define O_CREAT     0x0100
#define O_TRUNC     0x0200

extern void *dlopen(const char *filename, int flags);
extern void *dlsym(void *handle, const char *symbol);
extern char *dlerror(void);

extern int open(const char *pathname, int flags, ...);
extern ssize_t write(int fd, const void *buf, size_t count);
extern int close(int fd);
extern int snprintf(char *str, size_t size, const char *format, ...);
extern pid_t getpid(void);
extern ssize_t readlink(const char *pathname, char *buf, size_t bufsiz);
extern int strcmp(const char *s1, const char *s2);
extern int access(const char *pathname, int mode);

static void write_file(const char *path, const char *msg, size_t len) {
    int fd = open(path, O_WRONLY | O_CREAT | O_APPEND, 0666);
    if (fd >= 0) {
        write(fd, msg, len);
        close(fd);
    }
}

static void log_append(const char *msg) {
    size_t len = 0;
    while (msg[len]) len++;
    /* Write ONLY to RAM tmpfs to prevent SD card I/O storms and FAT corruption */
    write_file("/tmp/disc-ui-hook.log", msg, len);
}

static void *g_orig_handle = (void*)0;
static void *(*g_orig_hwaccel_api)(int ver) = (void*)0;
static int (*g_orig_pc_usleep)(unsigned int usec) = (void*)0;

/* Exported symbol: disc_hwaccel_get_api */
void *disc_hwaccel_get_api(int ver) {
    if (g_orig_hwaccel_api) {
        return g_orig_hwaccel_api(ver);
    }
    return (void*)0;
}

/* mq_ui function pointers */
typedef void* (*lv_list_add_btn_t)(void *list, const char *text, int index, void *callback, void *extra);
static lv_list_add_btn_t lv_list_add_btn = (lv_list_add_btn_t)0x00485ef8;

/* mq_ui container helper: get child at index (-1 = last added child) */
typedef void* (*lv_list_get_btn_t)(void *list, int index);
static lv_list_get_btn_t lv_list_get_btn = (lv_list_get_btn_t)0x004f8850;

typedef void* (*lv_obj_valid_t)(void *obj);
static lv_obj_valid_t lv_obj_valid = (lv_obj_valid_t)0x004e9a70;

typedef void (*lv_btn_set_state_t)(void *btn, int state);
static lv_btn_set_state_t lv_btn_set_state = (lv_btn_set_state_t)0x004e96d0;

typedef void (*lv_obj_set_event_cb_t)(void *obj, void *callback, int filter, void *user_data);
static lv_obj_set_event_cb_t lv_obj_set_event_cb = (lv_obj_set_event_cb_t)0x004e39ec;

typedef void* (*lv_event_get_target_t)(void *e);
static lv_event_get_target_t lv_event_get_target = (lv_event_get_target_t)0x004e3924;

typedef int (*lv_event_get_code_t)(void *e);
static lv_event_get_code_t lv_event_get_code = (lv_event_get_code_t)0x004e3934;

typedef void (*dlna_enter_native_t)(int mode);
static dlna_enter_native_t dlna_enter_native = (dlna_enter_native_t)0x0046f29c;

/* Button state */
static void *g_last_list = (void*)0;
static void *g_podcast_btn = (void*)0;
static void *g_radio_btn = (void*)0;

static void send_ipc_click(const char *source) {
    int fd = open("/tmp/disc-podcast-radio/click", O_WRONLY | O_CREAT | O_TRUNC, 0666);
    if (fd >= 0) {
        size_t len = 0;
        while (source[len]) len++;
        write(fd, source, len);
        write(fd, "\n", 1);
        close(fd);
    }
}

static void btn_event_cb(void *e) {
    int code = lv_event_get_code(e);
    void *target = lv_event_get_target(e);

    /* Event 7 = LV_EVENT_CLICKED in mq_ui LVGL */
    if (code == 7) {
        if (target == g_podcast_btn) {
            log_append("[disc-ui-hook] Clicked: 播客 (Podcast)\n");
            send_ipc_click("podcast");
            dlna_enter_native(1);
        } else if (target == g_radio_btn) {
            log_append("[disc-ui-hook] Clicked: 网络收音机 (Radio)\n");
            send_ipc_click("radio");
            dlna_enter_native(1);
        }
    }
    /* Event 0x21 (33) = LV_EVENT_DELETE */
    else if (code == 0x21) {
        if (target == g_podcast_btn) {
            g_podcast_btn = (void*)0;
            log_append("[disc-ui-hook] Reset podcast button\n");
        } else if (target == g_radio_btn) {
            g_radio_btn = (void*)0;
            log_append("[disc-ui-hook] Reset radio button\n");
        }
    }
}

static int my_pc_usleep(unsigned int usec) {
    int ret = 0;

    /* 1. Run stock / module-ui-orig usleep first so native UI items are processed */
    if (g_orig_pc_usleep) {
        ret = g_orig_pc_usleep(usec);
    }

    /* 2. Safety kill-switch check */
    if (access("/tmp/disable_ui_hook", 0) != 0) {
        void **list_ptr = (void**)0x008de1b0;
        void *list = *list_ptr;

        /* Check if streaming list is currently active and valid */
        if (list != (void*)0 && lv_obj_valid(list)) {
            /* If streaming list pointer changed, reset tracking */
            if (list != g_last_list) {
                g_last_list = list;
                g_podcast_btn = (void*)0;
                g_radio_btn = (void*)0;
            }

            /* Add "播客" button */
            if (!g_podcast_btn) {
                lv_list_add_btn(list, "播客", 0, btn_event_cb, (void*)0);
                /* Crucial fix: obtain the actual lv_btn pointer from the list */
                g_podcast_btn = lv_list_get_btn(list, -1);
                if (g_podcast_btn) {
                    lv_btn_set_state(g_podcast_btn, 0x4000);
                    /* Register for delete cleanup (event 0x21) */
                    lv_obj_set_event_cb(g_podcast_btn, btn_event_cb, 0x21, (void*)0);
                    log_append("[disc-ui-hook] Added: 播客\n");
                }
            }

            /* Add "网络收音机" button */
            if (!g_radio_btn) {
                lv_list_add_btn(list, "网络收音机", 0, btn_event_cb, (void*)0);
                /* Crucial fix: obtain the actual lv_btn pointer from the list */
                g_radio_btn = lv_list_get_btn(list, -1);
                if (g_radio_btn) {
                    lv_btn_set_state(g_radio_btn, 0x4000);
                    /* Register for delete cleanup (event 0x21) */
                    lv_obj_set_event_cb(g_radio_btn, btn_event_cb, 0x21, (void*)0);
                    log_append("[disc-ui-hook] Added: 网络收音机\n");
                }
            }
        }
    }

    return ret;
}

/* Constructor */
void __attribute__((constructor)) disc_ui_hook_init(void) {
    char exe_buf[128];
    ssize_t n = readlink("/proc/self/exe", exe_buf, sizeof(exe_buf) - 1);
    if (n > 0) {
        exe_buf[n] = '\0';
    } else {
        exe_buf[0] = '\0';
    }

    char buf[256];
    snprintf(buf, sizeof(buf), "[disc-ui-hook] Constructor running in PID %d, exe: %s\n", (int)getpid(), exe_buf);
    log_append(buf);

    /* Only initialize inside mq_ui */
    if (strcmp(exe_buf, "/usr/bin/mq_ui") != 0 && strcmp(exe_buf, "/usr/data/mq_ui") != 0) {
        log_append("[disc-ui-hook] Not mq_ui process, skipping hook.\n");
        return;
    }

    log_append("[disc-ui-hook] Loading original module-ui-orig.so...\n");
    g_orig_handle = dlopen("/usr/data/disc-settings/module-ui-orig.so", RTLD_NOW | RTLD_GLOBAL);
    if (!g_orig_handle) {
        /* Fallback to /usr/data/disc-settings.so */
        g_orig_handle = dlopen("/usr/data/disc-settings.so", RTLD_NOW | RTLD_GLOBAL);
    }

    if (!g_orig_handle) {
        const char *err = dlerror();
        snprintf(buf, sizeof(buf), "[disc-ui-hook] Failed to load original module-ui: %s\n", err ? err : "unknown");
        log_append(buf);
        return;
    }

    log_append("[disc-ui-hook] Original module-ui loaded successfully!\n");
    g_orig_hwaccel_api = (void*(*)(int))dlsym(g_orig_handle, "disc_hwaccel_get_api");

    /* Install usleep hook in mq_ui .got.plt */
    void **usleep_got = (void**)0x0083a114;
    g_orig_pc_usleep = (int(*)(unsigned int))*usleep_got;
    snprintf(buf, sizeof(buf), "[disc-ui-hook] Original pc_usleep = %p\n", (void*)g_orig_pc_usleep);
    log_append(buf);

    if (g_orig_pc_usleep) {
        *usleep_got = (void*)my_pc_usleep;
        log_append("[disc-ui-hook] Hook installed: usleep (0x83a114) -> my_pc_usleep\n");
    }
}
