# Turns the output of `getfattr -R -h -d -m - -e hex` into lines of
# "path<TAB>name<TAB>hexvalue" (the value without its 0x prefix).
/^# file: / { f = substr($0, 9); next }
/^[a-z]+\.[^=]+=0x/ {
  i = index($0, "=")
  printf "%s\t%s\t%s\n", f, substr($0, 1, i - 1), substr($0, i + 3)
}
/^[a-z]+\.[^=]+$/ {
  printf "%s\t%s\t\n", f, $0
}
