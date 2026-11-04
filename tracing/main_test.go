package tracing_test

import (
	"testing"

	"go.uber.org/goleak"
)

// The batch span processor runs a goroutine per provider; Shutdown must end it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
