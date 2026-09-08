package authsrv

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// TestMetadataDecodificaNoSDK é o teste que amarra o documento que o patchbay
// publica ao que o cliente lê: MetadataServidor é struct própria (para não
// prometer um jwks_uri que não existe), e é este round-trip que garante que os
// nomes de campo não deriva­ram dos do go-sdk.
func TestMetadataDecodificaNoSDK(t *testing.T) {
	t.Parallel()

	sut := servicoDeTeste("https://patchbay.exemplo")
	bruto, err := json.Marshal(sut.Metadata([]string{"endpoint:pessoal"}))
	if err != nil {
		t.Fatalf("serializar metadata: erro = %v, quer nil", err)
	}

	// O jwks_uri não pode aparecer: o access token é opaco, não há chave
	// pública a publicar, e um campo vazio prometeria um JWKS inexistente.
	var cru map[string]any
	if err := json.Unmarshal(bruto, &cru); err != nil {
		t.Fatalf("decodificar metadata crua: erro = %v, quer nil", err)
	}
	if _, tem := cru["jwks_uri"]; tem {
		t.Error("metadata publica jwks_uri, e o access token deste AS é opaco")
	}

	var meta oauthex.AuthServerMeta
	if err := json.Unmarshal(bruto, &meta); err != nil {
		t.Fatalf("decodificar no struct do SDK: erro = %v, quer nil", err)
	}
	switch {
	case meta.Issuer != "https://patchbay.exemplo":
		t.Errorf("issuer = %q, quer https://patchbay.exemplo", meta.Issuer)
	case meta.AuthorizationEndpoint != "https://patchbay.exemplo/oauth/authorize":
		t.Errorf("authorization_endpoint = %q", meta.AuthorizationEndpoint)
	case meta.TokenEndpoint != "https://patchbay.exemplo/oauth/token":
		t.Errorf("token_endpoint = %q", meta.TokenEndpoint)
	case len(meta.CodeChallengeMethodsSupported) != 1:
		t.Errorf("code_challenge_methods_supported = %v, quer só S256", meta.CodeChallengeMethodsSupported)
	}
}
