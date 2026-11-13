package claudecode

import (
	"os"
	"testing"
)

// Tests run from inside a cp3 session inherit its CLAUDE_PEERS_AGENT, which
// outranks the directory identity every identity test expects.
func TestMain(m *testing.M) {
	os.Unsetenv("CLAUDE_PEERS_AGENT")
	os.Exit(m.Run())
}
