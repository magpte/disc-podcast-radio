import subprocess
import os
import sys

src_file = r"e:\DISC\_内部\src\disc-ui-hook\disc_ui_hook.c"
out_dir = r"e:\DISC\_内部\build"
out_so = os.path.join(out_dir, "module-ui.so")
lib_dir = r"e:\DISC\_内部\extracted\rootfs\lib"

libc_path = os.path.join(lib_dir, "libc-2.29.so")
libdl_path = os.path.join(lib_dir, "libdl-2.29.so")

clang_path = r"C:\Program Files\LLVM\bin\clang.exe"

os.makedirs(out_dir, exist_ok=True)

cmd = [
    clang_path,
    "--target=mipsel-linux-gnu",
    "-mnan=2008",
    "-fuse-ld=lld",
    "-shared",
    "-fPIC",
    "-O2",
    "-nostdlib",
    libc_path,
    libdl_path,
    "-o", out_so,
    src_file
]

print("Compiling disc_ui_hook.c with -mnan=2008...")
res = subprocess.run(cmd, capture_output=True, text=True)
if res.returncode != 0:
    print("Compilation FAILED!")
    print("STDOUT:", res.stdout)
    print("STDERR:", res.stderr)
    sys.exit(1)

print("Compilation SUCCESS:", out_so)
file_size = os.path.getsize(out_so)
print(f"Binary size: {file_size} bytes")

# Also compile test_loader
loader_src = r"e:\DISC\_内部\src\disc-ui-hook\test_loader.c"
loader_bin = os.path.join(out_dir, "test_loader")

loader_cmd = [
    clang_path,
    "--target=mipsel-linux-gnu",
    "-mnan=2008",
    "-fuse-ld=lld",
    "-O2",
    "-nostdlib",
    "-Wl,-e,_start",
    "-Wl,--dynamic-linker=/lib/ld-linux-mipsn8.so.1",
    libc_path,
    libdl_path,
    "-o", loader_bin,
    loader_src
]

print("Compiling test_loader.c with -mnan=2008...")
res2 = subprocess.run(loader_cmd, capture_output=True, text=True)
if res2.returncode != 0:
    print("test_loader Compilation FAILED!")
    print("STDOUT:", res2.stdout)
    print("STDERR:", res2.stderr)
    sys.exit(1)

print("test_loader SUCCESS:", loader_bin)
print(f"test_loader size: {os.path.getsize(loader_bin)} bytes")

# Deploy to package directory and rebuild zips
import shutil, hashlib
deploy_dir = r"e:\DISC\TF卡免刷机安装包 (推荐)\DISC_MODULES\disc-podcast-radio\1.0.5"
for fn in ["module-ui.so", "test_loader"]:
    shutil.copy2(os.path.join(out_dir, fn), os.path.join(deploy_dir, fn))
    print(f"Copied {fn} -> {deploy_dir}")

# Update SHA256SUMS
lines = []
for fname in sorted(os.listdir(deploy_dir)):
    if fname == "SHA256SUMS":
        continue
    fpath = os.path.join(deploy_dir, fname)
    if os.path.isfile(fpath):
        with open(fpath, "rb") as f:
            digest = hashlib.sha256(f.read()).hexdigest()
        lines.append(f"{digest}  {fname}\n")

with open(os.path.join(deploy_dir, "SHA256SUMS"), "wb") as f:
    f.write("".join(lines).encode("utf-8"))
print("Updated SHA256SUMS.")

# Rebuild release zips
res_zip = subprocess.run(["python", r"e:\DISC\_内部\package_zips.py"], capture_output=True, text=True)
print(res_zip.stdout)

