package runner

import (
	"encoding/json"
	"fmt"

	"github.com/pacenote-sim/protocol/wire"

	"github.com/pacenote-sim/client/internal/api"
)

// unmarshal reads a queued body. A body that cannot be read is a write that
// can never be sent, so it comes back as an invalid answer and the queue drops it.
func unmarshal(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return &api.Error{Code: wire.CodeInvalid, Message: fmt.Sprintf("a queued write could not be read: %v", err)}
	}
	return nil
}
