(exit 3); echo st=$?
sh -c 'kill -TERM $$'; echo sig=$?
# The shell reports the killed background job as "Terminated" or "<script>:
# Terminated", depending on whether it reaps it before or inside wait: not shown.
# (The case runs with -sysmounts: the background job's stdin is /dev/null, and
# without it the job exited 2 before the kill when it won the race.)
exec 3>&2 2>/dev/null
sleep 30 &
p=$!
kill $p
wait $p; echo bg=$?
exec 2>&3
true | false | (exit 2); echo pipe=$?
echo done
