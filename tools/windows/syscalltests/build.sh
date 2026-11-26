#!/usr/bin/env bash
# build.sh OUT: builds gVisor's syscall tests from google/gvisor at the commit
# the go branch was generated from (ADR 0009) as static binaries with Bazel.
# Env: TARGETS (default: every syscall_test of test/syscalls/BUILD, plus the
# helper binaries of test/syscalls/linux), BAZEL_FLAGS, SHA.
set -euxo pipefail
OUT=$(realpath -m "$1")
SHA=${SHA:-c3efd8c9381c89b78c1d86b6c64a49d53ee938f8}
UP=${UP:-$HOME/gvisor-upstream}
mkdir -p "$OUT/bin" "$UP"
cd "$UP"
if [ ! -d .git ]; then
  git init -q .
  git remote add origin https://github.com/google/gvisor
fi
git fetch -q --depth 1 origin "$SHA"
git checkout -q --force FETCH_HEAD
git log -1 --format='upstream %H %cd' | tee "$OUT/upstream-version.txt"

# The tests are the syscall_test() entries; the other cc_binary targets of the
# directory are helpers that tests execute.
grep -o 'test = "//test/syscalls/linux:[^"]*"' test/syscalls/BUILD | sed 's/.*://; s/"//' | sort -u > "$OUT/tests.txt"
bazel query 'kind("cc_binary", //test/syscalls/linux:*)' 2>/dev/null | sed 's#.*:##' | sort -u > "$OUT/binaries.txt" || cp "$OUT/tests.txt" "$OUT/binaries.txt"
comm -13 "$OUT/tests.txt" "$OUT/binaries.txt" > "$OUT/helpers.txt"
wc -l "$OUT/tests.txt" "$OUT/helpers.txt"

if [ -z "${TARGETS:-}" ]; then
  TARGETS=$(cat "$OUT/tests.txt" "$OUT/helpers.txt" | sed 's#^#//test/syscalls/linux:#' | tr '\n' ' ')
fi

# Static: the same link options as tools/bazeldefs/cc.bzl uses for static=True.
bazel build --config=x86_64 -c opt --keep_going --noshow_progress --color=no \
  --disk_cache="$HOME/.cache/bazel-disk" --repository_cache="$HOME/.cache/bazel-repo" \
  --action_env=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin --host_action_env=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin --linkopt=-B/usr/bin --verbose_failures \
  --strip=always --copt=-w --cxxopt=-w --host_copt=-w --host_cxxopt=-w \
  --linkopt=-static --linkopt=-lstdc++ --linkopt=-Wl,--whole-archive --linkopt=-lpthread --linkopt=-Wl,--no-whole-archive \
  ${BAZEL_FLAGS:-} $TARGETS 2>&1 | tee "$OUT/bazel.log" | tail -n 200 || true

{
  echo "--- debug: linker"
  ls -l /usr/bin/ld /usr/bin/ld.* /usr/bin/x86_64-linux-gnu-ld* 2>&1 | head -20
  gcc -print-prog-name=ld
  f=$(find "$HOME/.cache/bazel" -name "*_test-0.params" 2>/dev/null | head -1)
  echo "$f"
  head -c 2500 "$f"
  echo
} 2>&1 | tee "$OUT/debug-ld.txt" || true

BIN=$(bazel info bazel-bin --config=x86_64 2>/dev/null)/test/syscalls/linux
for t in $TARGETS; do
  n=${t##*:}
  if [ -f "$BIN/$n" ]; then cp -L "$BIN/$n" "$OUT/bin/$n"; else echo "MISSING $n" >> "$OUT/missing.txt"; fi
done
chmod +x "$OUT"/bin/* || true
ls "$OUT/bin" | wc -l
file "$OUT/bin/"* | sed 's/,.*//' | awk '{print $NF}' | sort | uniq -c || true
