package p

import (
	"fmt"
	"log"
	"os"
)

func f(w *os.File) {
	fmt.Fprintln(os.Stderr, "counted")
	fmt.Fprintf(os.Stdout, "stdout")
	fmt.Println("stdout")
	fmt.Fprint(w, "not seen: a variable")
	log.Printf("counted")
	log.Fatalln("counted")
	_ = fmt.Sprint("no writer")
}
