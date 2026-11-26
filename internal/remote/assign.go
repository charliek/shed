package remote

import (
	"context"

	"github.com/charliek/shed/internal/config"
)

// AssignUser is not supported on the remote backend: a cloud shed's owner
// is its machine's, fixed when the account service made it, and the compute
// contract has no assign (plan 041 §0.1 item 16). It answers 501
// FEATURE_UNSUPPORTED.
func (b *Backend) AssignUser(context.Context, string, config.AssignShedRequest) (*config.Shed, error) {
	return nil, unsupported("assign (a cloud shed belongs to the person or client its machine was made for)")
}
