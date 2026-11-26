package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	fmt.Println("hello")
	if len(os.Args) > 1 {
		out, _ := exec.Command("/bin/true").CombinedOutput()
		fmt.Print(string(out))
	}
}
