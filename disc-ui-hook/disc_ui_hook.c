/*
 * disc_ui_hook.c - DISC UI Pure Text Hierarchical Menu Hook
 * Cross-compiled for MIPS32 little-endian (mipsel-linux-gnu, glibc 2.29, o32 ABI)
 */

#define UI_HOOK_VERSION "1.0.7"

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
extern ssize_t read(int fd, void *buf, size_t count);
extern int close(int fd);
extern int snprintf(char *str, size_t size, const char *format, ...);
extern pid_t getpid(void);
extern ssize_t readlink(const char *pathname, char *buf, size_t bufsiz);
extern int strcmp(const char *s1, const char *s2);
extern int strncmp(const char *s1, const char *s2, size_t n);
extern int access(const char *pathname, int mode);
extern void *memset(void *s, int c, size_t n);
extern void *memcpy(void *dest, const void *src, size_t n);
extern char *strncpy(char *dest, const char *src, size_t n);

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
    /* Write to RAM */
    write_file("/tmp/disc-ui-hook.log", msg, len);
    /* Write to TF card for direct user inspection */
    write_file("/tmp/sdcard/disc-ui-hook.log", msg, len);
}

/* Original dlopen handle */
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

/*
 * mq_ui function pointers
 * Note: In mq_ui 0x00485ef8, calling convention is:
 *   a0: list (void*)
 *   a1: text (const char*)   <-- CRITICAL: a1 is text string!
 *   a2: img / index (void*)
 *   a3: callback (void*)
 *   16(sp): extra (void*)
 */
typedef void* (*lv_list_add_btn_t)(void *list, const char *text, const void *img, void *callback, void *extra);
static lv_list_add_btn_t lv_list_add_btn = (lv_list_add_btn_t)0x00485ef8;

typedef void (*lv_list_clean_t)(void *list);
static lv_list_clean_t lv_list_clean = (lv_list_clean_t)0x00486920;

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

/* --- Menu State & Data Models --- */

typedef enum {
    MENU_MAIN = 0,         /* 主流媒体：原生保留网易云、QQ、酷狗，并追加播客、收音机 */
    MENU_PODCAST_FEEDS,    /* 播客列表：日谈公园、机核 GCORES 等 */
    MENU_PODCAST_EPS,      /* 播客单集：展示选中播客各期 */
    MENU_RADIO_CATS,       /* 电台分类：国家台、省市台、网络台 */
    MENU_RADIO_STATIONS    /* 电台列表：中国之声、经济之声等 */
} menu_state_t;

typedef enum {
    ACT_NONE = 0,
    ACT_GO_PODCASTS,
    ACT_GO_RADIOS,
    ACT_SELECT_FEED,
    ACT_PLAY_EPISODE,
    ACT_SELECT_RADIO_CAT,
    ACT_PLAY_STATION,
    ACT_BACK_TO_MAIN,
    ACT_BACK_TO_PODCASTS,
    ACT_BACK_TO_RADIO_CATS,
    ACT_NETEASE,
    ACT_QQ,
    ACT_KUGOU
} action_type_t;

typedef struct {
    void *btn;
    action_type_t action;
    int index;
    char label[80];
    char url[256];
} btn_item_t;

#define MAX_ITEMS 64
static btn_item_t g_items[MAX_ITEMS];
static int g_item_count = 0;

static menu_state_t g_current_menu = MENU_MAIN;
static void *g_active_list = (void*)0;
static void *g_podcast_btn = (void*)0;
static void *g_radio_btn = (void*)0;
static int g_selected_feed = 0;
static int g_selected_radio_cat = 0;
static char g_playing_title[80] = {0};

/* --- Static Presets --- */

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
    {"CRI 轻松调频 EZFM", "http://sk.cri.cn/915.m3u8"},
};

static const station_data_t g_province_stations[] = {
    {"北京音乐广播 (FM97.4)", "http://audio.rbc.cn/live/101.m3u8"},
    {"上海动感101 (FM101.7)", "http://lhttp.qtfm.cn/live/20272/64k.mp3"},
    {"广东音乐之声 (FM99.3)", "http://lhttp.qtfm.cn/live/20268/64k.mp3"},
    {"江苏音乐广播 (FM89.7)", "http://lhttp.qtfm.cn/live/20270/64k.mp3"},
};

static const station_data_t g_network_stations[] = {
    {"Asia Hitz 亚洲流行", "http://listen.radionomy.com/asiahitz"},
    {"Lofi Hip Hop 放松电台", "http://stream.zeno.fm/0r0xa792kwzuv"},
};

static const radio_cat_data_t g_radio_cats[] = {
    {"国家台", g_national_stations, sizeof(g_national_stations)/sizeof(g_national_stations[0])},
    {"省市台", g_province_stations, sizeof(g_province_stations)/sizeof(g_province_stations[0])},
    {"网络台", g_network_stations, sizeof(g_network_stations)/sizeof(g_network_stations[0])},
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
    {"日谈公园: 音乐与生活对话", "https://feed.xyzfm.space/m4n6775z661p"},
};

static const episode_data_t g_gcores_eps[] = {
    {"机核 GCORES: 最新电台", "https://alioss.gcores.com/feeds/apple_podcast_v1.xml"},
    {"机核 GADIO: 游戏文化专栏", "https://alioss.gcores.com/feeds/apple_podcast_v1.xml"},
    {"机核 读库: 有声书精选", "https://alioss.gcores.com/feeds/apple_podcast_v1.xml"},
};

static const episode_data_t g_hzyh_eps[] = {
    {"忽左忽右: 最新一期对谈", "https://feed.xyzfm.space/5k25126804"},
    {"忽左忽右: 历史与文化", "https://feed.xyzfm.space/5k25126804"},
};

static const podcast_feed_data_t g_podcasts[] = {
    {"日谈公园", "ritan", g_ritan_eps, sizeof(g_ritan_eps)/sizeof(g_ritan_eps[0])},
    {"机核 GCORES", "gcores", g_gcores_eps, sizeof(g_gcores_eps)/sizeof(g_gcores_eps[0])},
    {"忽左忽右", "hzyh", g_hzyh_eps, sizeof(g_hzyh_eps)/sizeof(g_hzyh_eps[0])},
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
    strncpy(g_playing_title, title, sizeof(g_playing_title) - 1);
    g_playing_title[sizeof(g_playing_title) - 1] = '\0';
}

/* Forward declarations */
static void btn_event_cb(void *e);
static void render_sub_menu(void *list);

static void* add_btn(void *list, const char *text, action_type_t action, int idx, const char *url) {
    if (g_item_count >= MAX_ITEMS) return (void*)0;

    /* In mq_ui: a0=list, a1=text, a2=img/index, a3=callback, 16(sp)=extra */
    lv_list_add_btn(list, text, (void*)0, (void*)btn_event_cb, (void*)0);
    void *btn = lv_list_get_btn(list, -1);
    if (!btn) {
        log_append("[disc-ui-hook] lv_list_get_btn failed for: ");
        log_append(text);
        log_append("\n");
        return (void*)0;
    }

    lv_btn_set_state(btn, 0x4000);
    lv_obj_set_event_cb(btn, (void*)btn_event_cb, 0x21, (void*)0); /* catch delete */

    g_items[g_item_count].btn = btn;
    g_items[g_item_count].action = action;
    g_items[g_item_count].index = idx;
    strncpy(g_items[g_item_count].label, text, sizeof(g_items[g_item_count].label) - 1);
    if (url) {
        strncpy(g_items[g_item_count].url, url, sizeof(g_items[g_item_count].url) - 1);
    } else {
        g_items[g_item_count].url[0] = '\0';
    }
    g_item_count++;

    log_append("[disc-ui-hook] Added btn: ");
    log_append(text);
    log_append("\n");

    return btn;
}

/* Try loading dynamic episodes from /tmp/disc-podcast-radio/episodes.txt */
static int load_dynamic_episodes(void *list, const char *target_feed) {
    int fd = open("/tmp/disc-podcast-radio/episodes.txt", O_RDONLY);
    if (fd < 0) return 0;

    char buf[4096];
    ssize_t n = read(fd, buf, sizeof(buf) - 1);
    close(fd);
    if (n <= 0) return 0;
    buf[n] = '\0';

    int matched = 0;
    char *line = buf;
    int count = 0;

    while (*line && count < 20) {
        char *eol = line;
        while (*eol && *eol != '\n') eol++;
        if (*eol == '\n') {
            *eol = '\0';
            eol++;
        }

        /* Check section [FeedTitle] */
        if (line[0] == '[' ) {
            char *end_b = line + 1;
            while (*end_b && *end_b != ']') end_b++;
            if (*end_b == ']') {
                *end_b = '\0';
                matched = (strcmp(line + 1, target_feed) == 0);
            }
        } else if (matched && line[0] != '\0') {
            /* Format: title | url */
            char *sep = line;
            while (*sep && *sep != '|') sep++;
            if (*sep == '|') {
                *sep = '\0';
                char *t = line;
                char *u = sep + 1;
                while (*u == ' ') u++;
                int tlen = 0; while (t[tlen]) tlen++;
                while (tlen > 0 && (t[tlen-1] == ' ' || t[tlen-1] == '\r')) { t[tlen-1] = '\0'; tlen--; }

                add_btn(list, t, ACT_PLAY_EPISODE, count, u);
                count++;
            }
        }
        line = eol;
    }
    return count;
}

static void render_sub_menu(void *list) {
    if (!list || !lv_obj_valid(list)) return;

    /* Clean all items for sub-menu */
    lv_list_clean(list);
    g_item_count = 0;
    g_podcast_btn = (void*)0;
    g_radio_btn = (void*)0;

    switch (g_current_menu) {
    case MENU_PODCAST_FEEDS:
        log_append("[disc-ui-hook] Rendering MENU_PODCAST_FEEDS\n");
        add_btn(list, "< 返回流媒体 (v" UI_HOOK_VERSION ")", ACT_BACK_TO_MAIN, 0, "");
        for (int i = 0; i < (int)PODCAST_COUNT; i++) {
            add_btn(list, g_podcasts[i].feed_title, ACT_SELECT_FEED, i, "");
        }
        break;

    case MENU_PODCAST_EPS:
        log_append("[disc-ui-hook] Rendering MENU_PODCAST_EPS\n");
        add_btn(list, "< 返回播客列表", ACT_BACK_TO_PODCASTS, 0, "");
        if (g_selected_feed >= 0 && g_selected_feed < (int)PODCAST_COUNT) {
            const podcast_feed_data_t *f = &g_podcasts[g_selected_feed];
            if (!load_dynamic_episodes(list, f->feed_title)) {
                for (int i = 0; i < f->ep_count; i++) {
                    add_btn(list, f->episodes[i].title, ACT_PLAY_EPISODE, i, f->episodes[i].url);
                }
            }
        }
        break;

    case MENU_RADIO_CATS:
        log_append("[disc-ui-hook] Rendering MENU_RADIO_CATS\n");
        add_btn(list, "< 返回流媒体 (v" UI_HOOK_VERSION ")", ACT_BACK_TO_MAIN, 0, "");
        for (int i = 0; i < (int)RADIO_CAT_COUNT; i++) {
            add_btn(list, g_radio_cats[i].cat_name, ACT_SELECT_RADIO_CAT, i, "");
        }
        break;

    case MENU_RADIO_STATIONS:
        log_append("[disc-ui-hook] Rendering MENU_RADIO_STATIONS\n");
        add_btn(list, "< 返回电台分类", ACT_BACK_TO_RADIO_CATS, 0, "");
        if (g_selected_radio_cat >= 0 && g_selected_radio_cat < (int)RADIO_CAT_COUNT) {
            const radio_cat_data_t *cat = &g_radio_cats[g_selected_radio_cat];
            for (int i = 0; i < cat->count; i++) {
                add_btn(list, cat->stations[i].name, ACT_PLAY_STATION, i, cat->stations[i].url);
            }
        }
        break;

    default:
        break;
    }
}

static void restore_main_menu(void *list) {
    if (!list || !lv_obj_valid(list)) return;
    lv_list_clean(list);
    g_item_count = 0;
    g_podcast_btn = (void*)0;
    g_radio_btn = (void*)0;

    log_append("[disc-ui-hook] Restoring main streaming menu\n");
    add_btn(list, "网易云音乐", ACT_NETEASE, 0, "");
    add_btn(list, "QQ音乐", ACT_QQ, 0, "");
    add_btn(list, "酷狗音乐", ACT_KUGOU, 0, "");
    g_podcast_btn = add_btn(list, "播客 (v" UI_HOOK_VERSION ")", ACT_GO_PODCASTS, 0, "");
    g_radio_btn = add_btn(list, "网络收音机 (v" UI_HOOK_VERSION ")", ACT_GO_RADIOS, 0, "");
}

static void btn_event_cb(void *e) {
    int code = lv_event_get_code(e);
    void *target = lv_event_get_target(e);

    /* Event 7 = LV_EVENT_CLICKED */
    if (code == 7) {
        /* Direct checks for podcast and radio main buttons */
        if (target == g_podcast_btn) {
            log_append("[disc-ui-hook] Clicked: 播客 (v1.0.6)\n");
            g_current_menu = MENU_PODCAST_FEEDS;
            render_sub_menu(g_active_list);
            return;
        } else if (target == g_radio_btn) {
            log_append("[disc-ui-hook] Clicked: 网络收音机 (v1.0.6)\n");
            g_current_menu = MENU_RADIO_CATS;
            render_sub_menu(g_active_list);
            return;
        }

        for (int i = 0; i < g_item_count; i++) {
            if (target == g_items[i].btn) {
                action_type_t act = g_items[i].action;
                int idx = g_items[i].index;

                switch (act) {
                case ACT_GO_PODCASTS:
                    log_append("[disc-ui-hook] ACT_GO_PODCASTS\n");
                    g_current_menu = MENU_PODCAST_FEEDS;
                    render_sub_menu(g_active_list);
                    break;

                case ACT_GO_RADIOS:
                    log_append("[disc-ui-hook] ACT_GO_RADIOS\n");
                    g_current_menu = MENU_RADIO_CATS;
                    render_sub_menu(g_active_list);
                    break;

                case ACT_SELECT_FEED:
                    log_append("[disc-ui-hook] ACT_SELECT_FEED: ");
                    log_append(g_items[i].label);
                    log_append("\n");
                    g_selected_feed = idx;
                    g_current_menu = MENU_PODCAST_EPS;
                    render_sub_menu(g_active_list);
                    break;

                case ACT_PLAY_EPISODE:
                    log_append("[disc-ui-hook] Play episode: ");
                    log_append(g_items[i].label);
                    log_append("\n");
                    send_play_command("podcast", g_items[i].label, g_items[i].url);
                    break;

                case ACT_SELECT_RADIO_CAT:
                    log_append("[disc-ui-hook] ACT_SELECT_RADIO_CAT: ");
                    log_append(g_items[i].label);
                    log_append("\n");
                    g_selected_radio_cat = idx;
                    g_current_menu = MENU_RADIO_STATIONS;
                    render_sub_menu(g_active_list);
                    break;

                case ACT_PLAY_STATION:
                    log_append("[disc-ui-hook] Play station: ");
                    log_append(g_items[i].label);
                    log_append("\n");
                    send_play_command("radio", g_items[i].label, g_items[i].url);
                    break;

                case ACT_BACK_TO_MAIN:
                    log_append("[disc-ui-hook] ACT_BACK_TO_MAIN\n");
                    g_current_menu = MENU_MAIN;
                    restore_main_menu(g_active_list);
                    break;

                case ACT_BACK_TO_PODCASTS:
                    log_append("[disc-ui-hook] ACT_BACK_TO_PODCASTS\n");
                    g_current_menu = MENU_PODCAST_FEEDS;
                    render_sub_menu(g_active_list);
                    break;

                case ACT_BACK_TO_RADIO_CATS:
                    log_append("[disc-ui-hook] ACT_BACK_TO_RADIO_CATS\n");
                    g_current_menu = MENU_RADIO_CATS;
                    render_sub_menu(g_active_list);
                    break;

                case ACT_NETEASE:
                    log_append("[disc-ui-hook] ACT_NETEASE\n");
                    dlna_enter_native(1);
                    break;

                case ACT_QQ:
                    log_append("[disc-ui-hook] ACT_QQ\n");
                    dlna_enter_native(2);
                    break;

                case ACT_KUGOU:
                    log_append("[disc-ui-hook] ACT_KUGOU\n");
                    dlna_enter_native(3);
                    break;

                default:
                    break;
                }
                break;
            }
        }
    }
    /* Event 0x21 (33) = LV_EVENT_DELETE */
    else if (code == 0x21) {
        if (target == g_podcast_btn) {
            g_podcast_btn = (void*)0;
        } else if (target == g_radio_btn) {
            g_radio_btn = (void*)0;
        }
        for (int i = 0; i < g_item_count; i++) {
            if (target == g_items[i].btn) {
                g_items[i].btn = (void*)0;
                break;
            }
        }
    }
}

static int my_pc_usleep(unsigned int usec) {
    int ret = 0;

    if (g_orig_pc_usleep) {
        ret = g_orig_pc_usleep(usec);
    }

    if (access("/tmp/disable_ui_hook", 0) != 0) {
        void **list_ptr = (void**)0x008de1b0;
        void *list = *list_ptr;

        if (list != (void*)0 && lv_obj_valid(list)) {
            /* Check if a new streaming list opened */
            if (list != g_active_list) {
                g_active_list = list;
                g_current_menu = MENU_MAIN;
                g_podcast_btn = (void*)0;
                g_radio_btn = (void*)0;
                g_item_count = 0;
                log_append("[disc-ui-hook] Streaming list detected at 0x8de1b0\n");
            }

            /* In MENU_MAIN, append 播客 and 网络收音机 to native list */
            if (g_current_menu == MENU_MAIN) {
                if (!g_podcast_btn) {
                    g_podcast_btn = add_btn(list, "播客 (v" UI_HOOK_VERSION ")", ACT_GO_PODCASTS, 0, "");
                }
                if (!g_radio_btn) {
                    g_radio_btn = add_btn(list, "网络收音机 (v" UI_HOOK_VERSION ")", ACT_GO_RADIOS, 0, "");
                }
            }
        } else {
            /* Left streaming view, reset active list */
            if (g_active_list != (void*)0) {
                log_append("[disc-ui-hook] Left streaming view, reset active list\n");
                g_active_list = (void*)0;
                g_current_menu = MENU_MAIN;
                g_podcast_btn = (void*)0;
                g_radio_btn = (void*)0;
                g_item_count = 0;
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
    snprintf(buf, sizeof(buf), "[disc-ui-hook] Pure text menu hook v" UI_HOOK_VERSION " init in PID %d, exe: %s\n", (int)getpid(), exe_buf);
    log_append(buf);

    if (strcmp(exe_buf, "/usr/bin/mq_ui") != 0 && strcmp(exe_buf, "/usr/data/mq_ui") != 0) {
        log_append("[disc-ui-hook] Not mq_ui process, skipping hook.\n");
        return;
    }

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
        log_append("[disc-ui-hook] Hook installed: usleep (0x83a114) -> pure text menu handler\n");
    }
}
