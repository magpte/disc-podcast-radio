/*
 * disc_ui_hook.c - DISC UI Pure Text Hierarchical Menu Hook
 * Strictly following fiio-disc-mod-firmware (X2000 / LVGL v8) production standards.
 *
 * Core Safety Principles:
 * 1. ZERO lv_list_clean or synchronous destruction inside event callbacks (No Use-After-Free).
 * 2. Independent sub-screen (lv_create(0) + screen_load) instead of modifying native lists.
 * 3. All LVGL calls strictly protected by pthread_mutex_trylock((void*)0x8d9e70).
 * 4. Tap guard (protect_tap & 0x4000 bubble flag) to distinguish swipes from taps.
 * 5. Full support for DISC_MODULES_SAFE_MODE bypass.
 */

#define UI_HOOK_VERSION "1.2.2"

typedef unsigned int size_t;
typedef int ssize_t;
typedef int pid_t;
typedef unsigned long uintptr_t;
typedef long intptr_t;

#define RTLD_LAZY   0x0001
#define RTLD_NOW    0x0002
#define RTLD_GLOBAL 0x0004

/* MIPS Linux open() flags */
#define O_RDONLY    0x0000
#define O_WRONLY    0x0001
#define O_RDWR      0x0002
#define O_APPEND    0x0008
#define O_CREAT     0x0100
#define O_TRUNC     0x0200

typedef void LvObj;

/* Standard C library functions */
extern void *dlopen(const char *filename, int flags);
extern void *dlsym(void *handle, const char *symbol);
extern char *dlerror(void);
extern int open(const char *pathname, int flags, ...);
extern ssize_t write(int fd, const void *buf, size_t count);
extern ssize_t read(int fd, void *buf, size_t count);
extern int close(int fd);
extern int snprintf(char *str, size_t size, const char *format, ...);
extern pid_t getpid(void);
extern ssize_t readlink(const char *pathname, char *buf, size_t bufsiz);
extern int strcmp(const char *s1, const char *s2);
extern int access(const char *pathname, int mode);
extern void *memset(void *s, int c, size_t n);
extern void *memcpy(void *dest, const void *src, size_t n);
extern char *strncpy(char *dest, const char *src, size_t n);
extern unsigned long pthread_self(void);
extern int pthread_mutex_trylock(void *mutex);
extern int pthread_mutex_unlock(void *mutex);
extern int clock_gettime(int clk_id, long *tp);

/* Macro helpers from fiio-disc-mod-firmware */
#define LV_PTR(a,t) ((t)(unsigned long)(a))
#define FN(a,t) LV_PTR(a,t)
#define LV_GLOBAL(a) (*(LvObj **)(unsigned long)(a))

#define lv_valid         LV_PTR(0x4e9a70, int (*)(const LvObj *))
#define lv_create        LV_PTR(0x4e969c, LvObj *(*)(LvObj *))
#define lv_btn           LV_PTR(0x52a8a4, LvObj *(*)(LvObj *))
#define lv_label         LV_PTR(0x52df64, LvObj *(*)(LvObj *))
#define lv_text          LV_PTR(0x52f2bc, void (*)(LvObj *, const char *))
#define lv_size          LV_PTR(0x4eb680, void (*)(LvObj *, int, int))
#define lv_align         LV_PTR(0x4ebc14, void (*)(LvObj *, int, int, int))
#define lv_add_flag      LV_PTR(0x4e96d0, void (*)(LvObj *, unsigned))
#define lv_clear_flag    LV_PTR(0x4e97f8, void (*)(LvObj *, unsigned))
#define lv_add_style     LV_PTR(0x4f45d0, void (*)(LvObj *, const void *, unsigned))
#define lv_color         LV_PTR(0x4f6df0, void (*)(LvObj *, unsigned, unsigned))
#define lv_bg            LV_PTR(0x4f6b50, void (*)(LvObj *, unsigned, unsigned))
#define lv_radius        LV_PTR(0x4f6e60, void (*)(LvObj *, int, unsigned))
#define lv_border        LV_PTR(0x4f6c40, void (*)(LvObj *, int, unsigned))
#define lv_add_cb        LV_PTR(0x4e39ec, void *(*)(LvObj *, void (*)(void *), unsigned, void *))
#define lv_event_code    LV_PTR(0x4e3934, unsigned (*)(const void *))
#define lv_event_target  LV_PTR(0x4e3924, LvObj *(*)(const void *))
#define lv_disp_default  LV_PTR(0x51a7e0, void *(*)(void))
#define lv_active_screen LV_PTR(0x4e26b4, LvObj *(*)(void *))

#define original_row     FN(0x485ef8, void (*)(LvObj *, const char *, unsigned, void (*)(void *), const int *))
#define screen_load      FN(0x4e2734, void (*)(LvObj *))
#define screen_delete    FN(0x4f7e98, void (*)(LvObj **))
#define screen_register  FN(0x486620, void (*)(LvObj *))
#define native_title     FN(0x46d88c, void (*)(LvObj *, const char *, unsigned))
#define child            FN(0x4f8850, LvObj *(*)(LvObj *, int))
#define child_count      FN(0x4f889c, unsigned (*)(LvObj *))
#define move_index       FN(0x4f8304, void (*)(LvObj *, int))
#define indev_active     FN(0x4e42e8, void *(*)(void))
#define indev_point      FN(0x4e45bc, void (*)(void *, int *))
#define bg_opa           FN(0x4f6b60, void (*)(LvObj *, unsigned, unsigned))
#define pad_top          FN(0x4f6af0, void (*)(LvObj *, int, unsigned))
#define pad_bottom       FN(0x4f6b00, void (*)(LvObj *, int, unsigned))
#define pad_left         FN(0x4f6b10, void (*)(LvObj *, int, unsigned))
#define pad_right        FN(0x4f6b20, void (*)(LvObj *, int, unsigned))

enum {
    DISC_LV_HIDDEN     = 1,
    DISC_LV_CLICKABLE  = 2,
    DISC_LV_SCROLLABLE = 16,
    DISC_LV_CLICKED    = 7,
    DISC_LV_DELETE     = 33
};

/* Logging exclusively to RAM tmpfs (non-blocking) */
static void log_append(const char *msg) {
    int fd = open("/tmp/disc-ui-hook.log", O_WRONLY | O_CREAT | O_APPEND, 0666);
    if (fd >= 0) {
        size_t len = 0;
        while (msg[len]) len++;
        write(fd, msg, len);
        close(fd);
    }
}

/* Original dlopen handle & function pointers */
static void *g_orig_handle = (void*)0;
static void *(*g_orig_hwaccel_api)(int ver) = (void*)0;
static int (*g_orig_pc_usleep)(unsigned int usec) = (void*)0;
static unsigned long g_main_thread = 0;

void *disc_hwaccel_get_api(int ver) {
    if (g_orig_hwaccel_api) {
        return g_orig_hwaccel_api(ver);
    }
    return (void*)0;
}

static unsigned milliseconds(void) {
    long t[2];
    if (clock_gettime(1, t)) return 0;
    return (unsigned)t[0] * 1000u + (unsigned)t[1] / 1000000u;
}

/* --- Official Tap Guard (protect_tap) --- */
static LvObj *tap_target = (void*)0;
static int tap_x = 0, tap_y = 0, tap_moved = 1, tap_released = 0;
static unsigned tap_started = 0;

static void tap_guard(void *e) {
    unsigned code = lv_event_code(e);
    LvObj *target = lv_event_target(e);
    int point[2];
    void *input = indev_active();

    if (code == 1) { /* LV_EVENT_PRESSED */
        tap_target = target;
        tap_moved = 1;
        tap_released = 0;
        tap_started = milliseconds();
        if (!input) return;
        indev_point(input, point);
        tap_x = point[0];
        tap_y = point[1];
        tap_moved = 0;
    } else if (target == tap_target) {
        if (input) {
            indev_point(input, point);
            int dx = point[0] - tap_x;
            int dy = point[1] - tap_y;
            if (dx > 8 || dx < -8 || dy > 8 || dy < -8) tap_moved = 1;
        } else {
            tap_moved = 1;
        }
        if (code == 8) { /* LV_EVENT_RELEASED */
            tap_released = 1;
        }
    }
}

static void protect_tap(LvObj *obj) {
    if (!obj) return;
    lv_add_flag(obj, 0x4000); /* Bubble gesture events through buttons */
    lv_add_cb(obj, tap_guard, 1, (void*)0);
    lv_add_cb(obj, tap_guard, 2, (void*)0);
    lv_add_cb(obj, tap_guard, 8, (void*)0);
}

static int stationary_clicked(void *e) {
    return (lv_event_code(e) == DISC_LV_CLICKED &&
            tap_target == lv_event_target(e) &&
            tap_released && !tap_moved);
}

/* UI Helper functions */
static LvObj *text_at(LvObj *p, const char *s, int y, unsigned color) {
    LvObj *l = lv_label(p);
    lv_add_style(l, (void *)0x83ac5c, 0);
    lv_color(l, color, 0);
    lv_text(l, s);
    lv_align(l, 9, 0, y);
    return l;
}

static LvObj *button_at(LvObj *p, const char *s, int x, int y, int width, void (*cb)(void *)) {
    LvObj *o = lv_btn(p);
    lv_size(o, width, 34);
    lv_align(o, 9, x, y);
    lv_border(o, 0, 0);
    lv_radius(o, 17, 0);
    lv_bg(o, 0xff333333, 0);
    text_at(o, s, 0, 0xffffffff);
    protect_tap(o);
    lv_add_cb(o, cb, DISC_LV_CLICKED, (void*)0);
    return o;
}

/* --- Menu Data Models --- */
typedef enum {
    VIEW_NONE = 0,
    VIEW_PODCAST_FEEDS,
    VIEW_PODCAST_EPS,
    VIEW_RADIO_CATS,
    VIEW_RADIO_STATIONS
} view_state_t;

typedef enum {
    ACT_NONE = 0,
    ACT_OPEN_FEED,
    ACT_OPEN_RADIO_CAT,
    ACT_PLAY_EPISODE,
    ACT_PLAY_STATION
} pending_action_t;

typedef struct {
    const char *name;
    const char *url;
} station_data_t;

typedef struct {
    const char *cat_name;
    const station_data_t *stations;
    int count;
} radio_cat_data_t;

static const station_data_t g_national_stations[] = {
    {"CNR-1 中国之声", "https://lhttp.qtfm.cn/live/15318317/64k.mp3"},
    {"CNR-2 经济之声", "http://ngcdn002.cnr.cn/live/jjzs/index.m3u8"},
    {"CNR-3 音乐之声", "http://ngcdn003.cnr.cn/live/yyzs/index.m3u8"},
    {"CNR-4 经典音乐广播", "http://ngcdn004.cnr.cn/live/jdyy/index.m3u8"},
    {"CNR-7 大湾区之声", "http://ngcdn007.cnr.cn/live/dwqzs/index.m3u8"},
    {"CRI 环球资讯广播", "http://sk.cri.cn/905.m3u8"},
    {"CRI 轻松调频 EZFM", "http://sk.cri.cn/915.m3u8"}
};

static const station_data_t g_province_stations[] = {
    {"北京音乐广播 (FM97.4)", "http://audio.rbc.cn/live/101.m3u8"},
    {"上海动感101 (FM101.7)", "http://lhttp.qtfm.cn/live/20272/64k.mp3"},
    {"广东音乐之声 (FM99.3)", "http://lhttp.qtfm.cn/live/20268/64k.mp3"},
    {"江苏音乐广播 (FM89.7)", "http://lhttp.qtfm.cn/live/20270/64k.mp3"}
};

static const station_data_t g_network_stations[] = {
    {"Asia Hitz 亚洲流行", "http://listen.radionomy.com/asiahitz"},
    {"Lofi Hip Hop 放松电台", "http://stream.zeno.fm/0r0xa792kwzuv"}
};

static const radio_cat_data_t g_radio_cats[] = {
    {"国家台", g_national_stations, sizeof(g_national_stations)/sizeof(g_national_stations[0])},
    {"省市台", g_province_stations, sizeof(g_province_stations)/sizeof(g_province_stations[0])},
    {"网络台", g_network_stations, sizeof(g_network_stations)/sizeof(g_network_stations[0])}
};
#define RADIO_CAT_COUNT (sizeof(g_radio_cats)/sizeof(g_radio_cats[0]))

typedef struct {
    const char *title;
    const char *url;
} episode_data_t;

typedef struct {
    const char *feed_title;
    const char *feed_id;
    const episode_data_t *episodes;
    int ep_count;
} podcast_feed_data_t;

static const episode_data_t g_ritan_eps[] = {
    {"日谈公园: 最新精选节目", "https://feed.xyzfm.space/m4n6775z661p"},
    {"日谈物语: 悬疑历史故事", "https://feed.xyzfm.space/m4n6775z661p"},
    {"日谈公园: 音乐与生活对话", "https://feed.xyzfm.space/m4n6775z661p"}
};

static const episode_data_t g_gcores_eps[] = {
    {"机核 GCORES: 最新电台", "https://alioss.gcores.com/feeds/apple_podcast_v1.xml"},
    {"机核 GADIO: 游戏文化专栏", "https://alioss.gcores.com/feeds/apple_podcast_v1.xml"},
    {"机核 读库: 有声书精选", "https://alioss.gcores.com/feeds/apple_podcast_v1.xml"}
};

static const episode_data_t g_hzyh_eps[] = {
    {"忽左忽右: 最新一期对谈", "https://feed.xyzfm.space/5k25126804"},
    {"忽左忽右: 历史与文化", "https://feed.xyzfm.space/5k25126804"}
};

static const podcast_feed_data_t g_podcasts[] = {
    {"日谈公园", "ritan", g_ritan_eps, sizeof(g_ritan_eps)/sizeof(g_ritan_eps[0])},
    {"机核 GCORES", "gcores", g_gcores_eps, sizeof(g_gcores_eps)/sizeof(g_gcores_eps[0])},
    {"忽左忽右", "hzyh", g_hzyh_eps, sizeof(g_hzyh_eps)/sizeof(g_hzyh_eps[0])}
};
#define PODCAST_COUNT (sizeof(g_podcasts)/sizeof(g_podcasts[0]))

/* Send play command to daemon */
static void send_play_command(const char *type, const char *title, const char *url) {
    int fd = open("/tmp/disc-podcast-radio/play_cmd", O_WRONLY | O_CREAT | O_TRUNC, 0666);
    if (fd >= 0) {
        char buf[512];
        int len = snprintf(buf, sizeof(buf), "%s\t%s\t%s\n", type, title, url);
        write(fd, buf, len);
        close(fd);
    }
}

/* --- UI State Variables --- */
static LvObj *g_podcast_entry_row = (void*)0;
static LvObj *g_radio_entry_row = (void*)0;

static LvObj *g_origin_screen = (void*)0;
static LvObj *g_sub_screen = (void*)0;
static view_state_t g_current_view = VIEW_NONE;

static int g_pending_open = 0;
static view_state_t g_requested_view = VIEW_NONE;

static int g_pending_back = 0;
static pending_action_t g_pending_action = ACT_NONE;
static int g_action_index = 0;

static int g_selected_feed = 0;
static int g_selected_cat = 0;
static int g_reentrant = 0;

/* Registered delete callbacks */
static void on_entry_row_deleted(void *e) {
    LvObj *t = lv_event_target(e);
    if (t == g_podcast_entry_row) g_podcast_entry_row = (void*)0;
    if (t == g_radio_entry_row)   g_radio_entry_row = (void*)0;
}

static void on_sub_screen_deleted(void *e) {
    LvObj *t = lv_event_target(e);
    if (t == g_sub_screen) {
        g_sub_screen = (void*)0;
        g_current_view = VIEW_NONE;
    }
}

static void on_origin_screen_deleted(void *e) {
    LvObj *t = lv_event_target(e);
    if (t == g_origin_screen) {
        g_origin_screen = (void*)0;
    }
}

/* Click callbacks for native streaming list entries */
static void podcast_entry_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    g_requested_view = VIEW_PODCAST_FEEDS;
    g_pending_open = 1;
}

static void radio_entry_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    g_requested_view = VIEW_RADIO_CATS;
    g_pending_open = 1;
}

/* Sub-screen click callbacks (Pure async state setting) */
static void back_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    g_pending_back = 1;
}

static void feed_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    LvObj *target = lv_event_target(e);
    for (int i = 0; i < (int)PODCAST_COUNT; i++) {
        LvObj *row = child(g_sub_screen, i + 1);
        if (target == row) {
            g_action_index = i;
            g_pending_action = ACT_OPEN_FEED;
            break;
        }
    }
}

static void cat_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    LvObj *target = lv_event_target(e);
    for (int i = 0; i < (int)RADIO_CAT_COUNT; i++) {
        LvObj *row = child(g_sub_screen, i + 1);
        if (target == row) {
            g_action_index = i;
            g_pending_action = ACT_OPEN_RADIO_CAT;
            break;
        }
    }
}

static void ep_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    LvObj *target = lv_event_target(e);
    if (g_selected_feed >= 0 && g_selected_feed < (int)PODCAST_COUNT) {
        int ep_cnt = g_podcasts[g_selected_feed].ep_count;
        for (int i = 0; i < ep_cnt; i++) {
            LvObj *row = child(g_sub_screen, i + 1);
            if (target == row) {
                g_action_index = i;
                g_pending_action = ACT_PLAY_EPISODE;
                break;
            }
        }
    }
}

static void station_clicked(void *e) {
    if (!stationary_clicked(e)) return;
    LvObj *target = lv_event_target(e);
    if (g_selected_cat >= 0 && g_selected_cat < (int)RADIO_CAT_COUNT) {
        int st_cnt = g_radio_cats[g_selected_cat].count;
        for (int i = 0; i < st_cnt; i++) {
            LvObj *row = child(g_sub_screen, i + 1);
            if (target == row) {
                g_action_index = i;
                g_pending_action = ACT_PLAY_STATION;
                break;
            }
        }
    }
}

/* Construct sub-screen cleanly using official mod patterns */
static void make_sub_screen(view_state_t view) {
    if (g_sub_screen && lv_valid(g_sub_screen)) {
        LvObj *old = g_sub_screen;
        g_sub_screen = (void*)0;
        screen_delete(&old);
    }

    g_current_view = view;
    g_sub_screen = lv_create(0);
    if (!g_sub_screen) return;

    screen_register(g_sub_screen);
    lv_add_cb(g_sub_screen, on_sub_screen_deleted, DISC_LV_DELETE, (void*)0);

    lv_size(g_sub_screen, 360, 360);
    lv_border(g_sub_screen, 0, 0);
    lv_bg(g_sub_screen, 0xff000000, 0);
    pad_top(g_sub_screen, 0, 0);
    pad_bottom(g_sub_screen, 0, 0);
    pad_left(g_sub_screen, 0, 0);
    pad_right(g_sub_screen, 0, 0);

    const char *title = "播客";
    if (view == VIEW_PODCAST_FEEDS) title = "播客列表";
    else if (view == VIEW_PODCAST_EPS) {
        if (g_selected_feed >= 0 && g_selected_feed < (int)PODCAST_COUNT) {
            title = g_podcasts[g_selected_feed].feed_title;
        } else {
            title = "播客单集";
        }
    } else if (view == VIEW_RADIO_CATS) title = "收音机分类";
    else if (view == VIEW_RADIO_STATIONS) {
        if (g_selected_cat >= 0 && g_selected_cat < (int)RADIO_CAT_COUNT) {
            title = g_radio_cats[g_selected_cat].cat_name;
        } else {
            title = "电台列表";
        }
    }

    /* Native Title bar */
    native_title(g_sub_screen, title, 0xb4fb);
    LvObj *tobj = child(g_sub_screen, 0);
    if (tobj) {
        lv_align(tobj, 1, 0, 44);
        lv_color(tobj, 0xff54baff, 0);
    }

    /* Standard Flex Column Flow for Vertical Scrolling */
    FN(0x4f0268, void (*)(LvObj *, int))(g_sub_screen, 0);
    FN(0x4f02c8, void (*)(LvObj *, int))(g_sub_screen, 12);
    FN(0x5030a0, void (*)(LvObj *, int))(g_sub_screen, 1);
    FN(0x5030e4, void (*)(LvObj *, int, int, int))(g_sub_screen, 0, 0, 2);

    /* Spacer for Title Bar */
    LvObj *top = lv_create(g_sub_screen);
    lv_size(top, 360, 40);
    lv_border(top, 0, 0);
    lv_bg(top, 0xff000000, 0);
    lv_clear_flag(top, DISC_LV_CLICKABLE | DISC_LV_SCROLLABLE);
    move_index(top, 0);

    int layout[3] = {44, 180, 0};

    if (view == VIEW_PODCAST_FEEDS) {
        for (int i = 0; i < (int)PODCAST_COUNT; i++) {
            unsigned n = child_count(g_sub_screen);
            original_row(g_sub_screen, g_podcasts[i].feed_title, i, feed_clicked, layout);
            LvObj *r = child(g_sub_screen, n);
            if (r) {
                lv_size(r, 360, 56);
                protect_tap(r);
            }
        }
    } else if (view == VIEW_PODCAST_EPS) {
        if (g_selected_feed >= 0 && g_selected_feed < (int)PODCAST_COUNT) {
            const podcast_feed_data_t *feed = &g_podcasts[g_selected_feed];
            for (int i = 0; i < feed->ep_count; i++) {
                unsigned n = child_count(g_sub_screen);
                original_row(g_sub_screen, feed->episodes[i].title, i, ep_clicked, layout);
                LvObj *r = child(g_sub_screen, n);
                if (r) {
                    lv_size(r, 360, 56);
                    protect_tap(r);
                }
            }
        }
    } else if (view == VIEW_RADIO_CATS) {
        for (int i = 0; i < (int)RADIO_CAT_COUNT; i++) {
            unsigned n = child_count(g_sub_screen);
            original_row(g_sub_screen, g_radio_cats[i].cat_name, i, cat_clicked, layout);
            LvObj *r = child(g_sub_screen, n);
            if (r) {
                lv_size(r, 360, 56);
                protect_tap(r);
            }
        }
    } else if (view == VIEW_RADIO_STATIONS) {
        if (g_selected_cat >= 0 && g_selected_cat < (int)RADIO_CAT_COUNT) {
            const radio_cat_data_t *cat = &g_radio_cats[g_selected_cat];
            for (int i = 0; i < cat->count; i++) {
                unsigned n = child_count(g_sub_screen);
                original_row(g_sub_screen, cat->stations[i].name, i, station_clicked, layout);
                LvObj *r = child(g_sub_screen, n);
                if (r) {
                    lv_size(r, 360, 56);
                    protect_tap(r);
                }
            }
        }
    }

    /* Fixed bottom footer with Back Button */
    LvObj *footer = lv_create(g_sub_screen);
    lv_size(footer, 360, 70);
    lv_border(footer, 0, 0);
    lv_bg(footer, 0xff000000, 0);
    lv_clear_flag(footer, DISC_LV_SCROLLABLE);
    lv_add_flag(footer, 0x4000);
    button_at(footer, "< 返回", 0, 0, 120, back_clicked);
    pad_bottom(g_sub_screen, 80, 0);

    screen_load(g_sub_screen);
    log_append("[disc-ui-hook] Sub-screen loaded successfully.\n");
}

/* Main Hook loop attached to mq_ui usleep GOT */
static int my_pc_usleep(unsigned int usec) {
    int ret = 0;
    if (g_orig_pc_usleep) {
        ret = g_orig_pc_usleep(usec);
    }

    /* Thread & Safe Mode check */
    if (access("/tmp/sdcard/DISC_MODULES_SAFE_MODE", 0) == 0) {
        return ret;
    }
    if (access("/tmp/disable_ui_hook", 0) == 0) {
        return ret;
    }

    if (g_main_thread && pthread_self() != g_main_thread) {
        return ret;
    }

    void *ra = __builtin_return_address(0);
    if ((uintptr_t)ra != 0x0040397c) {
        return ret;
    }

    /* Mutex check: LVGL is NOT thread-safe */
    if (!g_reentrant && !pthread_mutex_trylock((void*)0x8d9e70)) {
        g_reentrant = 1;

        /* 1. Ensure entry rows in native Streaming Media menu */
        LvObj *media = LV_GLOBAL(0x8de1b0);
        if (media && lv_valid(media)) {
            int layout[3] = {44, 180, 0};
            if (!g_podcast_entry_row) {
                unsigned n = child_count(media);
                original_row(media, "播客 (Podcast)", 0, podcast_entry_clicked, layout);
                if (child_count(media) > n) {
                    g_podcast_entry_row = child(media, -1);
                    protect_tap(g_podcast_entry_row);
                    lv_add_cb(g_podcast_entry_row, on_entry_row_deleted, DISC_LV_DELETE, (void*)0);
                    log_append("[disc-ui-hook] Attached 播客 row to streaming list\n");
                }
            }
            if (!g_radio_entry_row) {
                unsigned n = child_count(media);
                original_row(media, "网络收音机 (Radio)", 0, radio_entry_clicked, layout);
                if (child_count(media) > n) {
                    g_radio_entry_row = child(media, -1);
                    protect_tap(g_radio_entry_row);
                    lv_add_cb(g_radio_entry_row, on_entry_row_deleted, DISC_LV_DELETE, (void*)0);
                    log_append("[disc-ui-hook] Attached 网络收音机 row to streaming list\n");
                }
            }
        }

        /* 2. Process pending sub-screen opening */
        if (g_pending_open) {
            g_pending_open = 0;
            g_origin_screen = lv_active_screen(lv_disp_default());
            if (g_origin_screen) {
                lv_add_cb(g_origin_screen, on_origin_screen_deleted, DISC_LV_DELETE, (void*)0);
            }
            make_sub_screen(g_requested_view);
        }

        /* 3. Auto-cleanup sub-screen if user navigated away through gesture/hardware */
        if (g_sub_screen && lv_valid(g_sub_screen) && lv_active_screen(lv_disp_default()) != g_sub_screen) {
            LvObj *old = g_sub_screen;
            g_sub_screen = (void*)0;
            g_current_view = VIEW_NONE;
            screen_delete(&old);
        }

        /* 4. Process pending actions */
        if (g_pending_action != ACT_NONE) {
            pending_action_t act = g_pending_action;
            int idx = g_action_index;
            g_pending_action = ACT_NONE;

            if (act == ACT_OPEN_FEED) {
                g_selected_feed = idx;
                make_sub_screen(VIEW_PODCAST_EPS);
            } else if (act == ACT_OPEN_RADIO_CAT) {
                g_selected_cat = idx;
                make_sub_screen(VIEW_RADIO_STATIONS);
            } else if (act == ACT_PLAY_EPISODE) {
                if (g_selected_feed >= 0 && g_selected_feed < (int)PODCAST_COUNT &&
                    idx >= 0 && idx < g_podcasts[g_selected_feed].ep_count) {
                    const episode_data_t *ep = &g_podcasts[g_selected_feed].episodes[idx];
                    send_play_command("podcast", ep->title, ep->url);
                    log_append("[disc-ui-hook] Playing podcast episode: ");
                    log_append(ep->title);
                    log_append("\n");
                }
            } else if (act == ACT_PLAY_STATION) {
                if (g_selected_cat >= 0 && g_selected_cat < (int)RADIO_CAT_COUNT &&
                    idx >= 0 && idx < g_radio_cats[g_selected_cat].count) {
                    const station_data_t *st = &g_radio_cats[g_selected_cat].stations[idx];
                    send_play_command("radio", st->name, st->url);
                    log_append("[disc-ui-hook] Playing radio station: ");
                    log_append(st->name);
                    log_append("\n");
                }
            }
        }

        /* 5. Process pending back */
        if (g_pending_back) {
            g_pending_back = 0;
            if (g_current_view == VIEW_PODCAST_EPS) {
                make_sub_screen(VIEW_PODCAST_FEEDS);
            } else if (g_current_view == VIEW_RADIO_STATIONS) {
                make_sub_screen(VIEW_RADIO_CATS);
            } else {
                /* Return to origin screen */
                LvObj *old = g_sub_screen;
                g_sub_screen = (void*)0;
                g_current_view = VIEW_NONE;
                if (g_origin_screen && lv_valid(g_origin_screen)) {
                    screen_load(g_origin_screen);
                }
                if (old && lv_valid(old)) {
                    screen_delete(&old);
                }
                log_append("[disc-ui-hook] Returned to origin screen\n");
            }
        }

        g_reentrant = 0;
        pthread_mutex_unlock((void*)0x8d9e70);
    }

    return ret;
}

__attribute__((constructor)) static void disc_ui_hook_init(void) {
    if (access("/tmp/sdcard/DISC_MODULES_SAFE_MODE", 0) == 0) {
        log_append("[disc-ui-hook] SAFE MODE detected; skipping hook initialization.\n");
        return;
    }

    char exe_buf[256];
    ssize_t len = readlink("/proc/self/exe", exe_buf, sizeof(exe_buf) - 1);
    if (len > 0) {
        exe_buf[len] = '\0';
    } else {
        exe_buf[0] = '\0';
    }

    char buf[256];
    snprintf(buf, sizeof(buf), "[disc-ui-hook] Module v" UI_HOOK_VERSION " init in PID %d, exe: %s\n", (int)getpid(), exe_buf);
    log_append(buf);

    if (strcmp(exe_buf, "/usr/bin/mq_ui") != 0 && strcmp(exe_buf, "/usr/data/mq_ui") != 0) {
        log_append("[disc-ui-hook] Not mq_ui process, skipping hook.\n");
        return;
    }

    g_main_thread = pthread_self();

    /* Fallback chain for stock module */
    g_orig_handle = dlopen("/usr/data/disc-settings/module-ui-orig.so", RTLD_NOW | RTLD_GLOBAL);
    if (!g_orig_handle) {
        g_orig_handle = dlopen("/usr/data/disc-runtime/module-data/disc-podcast-radio/ui-backup/module-ui.so", RTLD_NOW | RTLD_GLOBAL);
    }
    if (!g_orig_handle) {
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
    if (g_orig_pc_usleep) {
        *usleep_got = (void*)my_pc_usleep;
        log_append("[disc-ui-hook] Hook installed: usleep (0x83a114) -> official sub-screen handler\n");
    }
}
