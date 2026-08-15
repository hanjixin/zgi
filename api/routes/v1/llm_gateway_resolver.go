package v1

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	apikeyrepo "github.com/zgiai/zgi/api/internal/modules/llm/apikey/repository"
	"github.com/zgiai/zgi/api/internal/util"
)

// gatewayKeyResolver resolves an organization's active, non-internal LLM
// gateway API key (the raw sk-... value) so codex/claude can authenticate
// against the ZGI LLM gateway. Usage meters through that key's quota.
type gatewayKeyResolver struct {
	repo apikeyrepo.APIKeyRepository
}

func (r *gatewayKeyResolver) ResolveGatewayKey(ctx context.Context, organizationID uuid.UUID) (string, error) {
	keys, _, err := r.repo.List(ctx, organizationID.String(), map[string]interface{}{
		"status":      "active",
		"is_internal": false,
	}, 1, 1)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", fmt.Errorf("no active llm api key for organization %s", organizationID)
	}
	return util.DecryptAPIKey(keys[0].Key)
}
