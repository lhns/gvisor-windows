printf 'b\nc\na\nb\n' | sort | uniq | grep -v c | sed s/b/X/
echo done
