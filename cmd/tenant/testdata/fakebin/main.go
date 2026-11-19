// Command fakebin stands in for a release `tenant` in TestUpdateFlow: it
// answers `version` with TENANT_FAKE_VERSION, and fails when
// TENANT_FAKE_FAIL=1, so the smoke test before the swap has something to
// catch. With TENANT_FAKE_PROTECTION set it also prints that as its
// process-protection report, the line `tenant version --protection` adds.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("fake", os.Getenv("TENANT_FAKE_VERSION"))
	if p := os.Getenv("TENANT_FAKE_PROTECTION"); p != "" {
		fmt.Println("protection: " + p)
	}
	if os.Getenv("TENANT_FAKE_FAIL") == "1" {
		os.Exit(1)
	}
}
