// greet says hello to $NAME (default "world"), proving env reaches the guest.
package main

import (
	"fmt"
	"os"
)

func main() {
	name := os.Getenv("NAME")
	if name == "" {
		name = "world"
	}
	fmt.Printf("Hello, %s!\n", name)
}
