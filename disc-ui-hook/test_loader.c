typedef unsigned int size_t;
typedef int ssize_t;

#define RTLD_LAZY   0x0001
#define RTLD_NOW    0x0002
#define RTLD_GLOBAL 0x0100

extern void *dlopen(const char *filename, int flags);
extern void *dlsym(void *handle, const char *symbol);
extern char *dlerror(void);
extern ssize_t write(int fd, const void *buf, size_t count);
extern int open(const char *pathname, int flags, ...);
extern int close(int fd);

static void print_str(const char *s) {
    if (!s) return;
    size_t len = 0;
    while (s[len]) len++;
    write(1, s, len);
}

int main(int argc, char **argv) {
    if (argc < 2) {
        print_str("Usage: test_loader <path_to_so>\n");
        return 1;
    }

    print_str("[test_loader] Testing dlopen on: ");
    print_str(argv[1]);
    print_str("\n");

    int fd = open(argv[1], 0);
    if (fd < 0) {
        print_str("[test_loader] WARNING: direct open() failed on target path\n");
    } else {
        close(fd);
    }

    void *h = dlopen(argv[1], RTLD_NOW | RTLD_GLOBAL);
    if (!h) {
        print_str("[test_loader] FAILED to load: ");
        const char *err = dlerror();
        print_str(err ? err : "unknown error");
        print_str("\n");
        return 1;
    }

    void *sym = dlsym(h, "disc_hwaccel_get_api");
    if (!sym) {
        print_str("[test_loader] WARNING: disc_hwaccel_get_api not found\n");
    } else {
        print_str("[test_loader] Symbol check OK: disc_hwaccel_get_api found\n");
    }

    print_str("[test_loader] SUCCESS! Library loaded cleanly without crash.\n");
    return 0;
}

__asm__(
    ".text\n"
    ".globl _start\n"
    ".type _start, @function\n"
    ".ent _start\n"
    "_start:\n"
    "    .set noreorder\n"
    "    .cpload $25\n"
    "    .set reorder\n"
    "    lw $4, 0($sp)\n"
    "    addiu $5, $sp, 4\n"
    "    addiu $1, $zero, -8\n"
    "    and $sp, $sp, $1\n"
    "    addiu $sp, $sp, -32\n"
    "    la $25, main\n"
    "    jalr $25\n"
    "    move $4, $2\n"
    "    li $2, 4001\n"
    "    syscall\n"
    ".end _start\n"
);
