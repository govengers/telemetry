package pgxadapter_test

import (
	"testing"

	"go.uber.org/goleak"
)

// The fake server of the tracer tests runs a goroutine per connection; closing the connection must end it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
