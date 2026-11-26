(exit 3); echo st=$?
sh -c 'kill -TERM $$'; echo sig=$?
sleep 30 &
p=$!
kill $p
wait $p; echo bg=$?
true | false | (exit 2); echo pipe=$?
echo done
