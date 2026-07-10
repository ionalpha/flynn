//go:build token

package main

import (
	"fmt"
	"os"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/ionalpha/flynn/extensions/token"
	"github.com/ionalpha/flynn/mission"
)

// init registers the token capability's tools. This file compiles only under the
// "token" build tag, so the default binary neither registers these tools nor links
// any cryptocurrency dependencies.
func init() { optionalToolProviders = append(optionalToolProviders, tokenTools) }

// tokenTools builds the token capability's tools when a signer is configured. With no
// signer configured (FLYNN_SOLANA_KEYPAIR unset) the capability is simply absent. A
// signer that is CONFIGURED but unreadable is an error, so a misconfiguration fails
// startup loudly instead of silently dropping the capability. The signer is read from
// a keypair file as a bootstrap; the engine takes a Signer interface, so a vault- or
// hardware-backed signer replaces it for production use.
func tokenTools() ([]mission.Tool, error) {
	keypair := os.Getenv("FLYNN_SOLANA_KEYPAIR")
	if keypair == "" {
		return nil, nil
	}
	payer, err := solana.PrivateKeyFromSolanaKeygenFile(keypair)
	if err != nil {
		return nil, fmt.Errorf("token capability: FLYNN_SOLANA_KEYPAIR is set but the keypair could not be read: %w", err)
	}
	endpoint := os.Getenv("FLYNN_SOLANA_RPC")
	if endpoint == "" {
		endpoint = rpc.DevNet_RPC
	}
	return token.Tools(token.NewEngine(rpc.New(endpoint), token.KeySigner{Key: payer})), nil
}
