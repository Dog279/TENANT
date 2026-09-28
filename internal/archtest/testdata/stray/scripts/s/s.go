package s

import (
	"fmt"
	"os"
)

func f() { fmt.Fprintln(os.Stderr, "outside cmd and internal") }
