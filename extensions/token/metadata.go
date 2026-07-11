//go:build token

package token

import (
	"context"
	"fmt"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// CreateMetadata attaches Metaplex metadata (name/symbol/logo+links URI) to a mint.
// It requires the mint authority to sign, so it must run BEFORE the mint authority
// is revoked. The metadata is created IMMUTABLE (is_mutable=false): once a token is
// reported safe, its identity can never be repainted into another project's name/symbol,
// which is a common impersonation vector when the update authority is a hot key.
func (e *Engine) CreateMetadata(ctx context.Context, mint solana.PublicKey, name, symbol, uri string) error {
	pda, err := metadataPDA(mint)
	if err != nil {
		return fmt.Errorf("metadata PDA: %w", err)
	}
	pk := e.payer.PublicKey()
	// accounts: metadata(w), mint, mintAuthority(s), payer(s,w), updateAuthority, systemProgram
	accts := solana.AccountMetaSlice{
		solana.Meta(pda).WRITE(),
		solana.Meta(mint),
		solana.Meta(pk).SIGNER(),
		solana.Meta(pk).SIGNER().WRITE(),
		solana.Meta(pk),
		solana.Meta(solana.SystemProgramID),
	}
	inst := solana.NewInstruction(metadataProgram, accts, createV3Data(name, symbol, uri))
	// Wait for FINALIZED: the mint's success report asserts this name/symbol, but the final
	// verify only re-reads the mint account, not the metadata. If the metadata landed only
	// at confirmed and its slot were later forked out while the supply/revoke land on the
	// canonical chain, the token would be reported safe with a name it does not actually
	// carry. Finalizing the metadata before continuing makes it durable and un-forkable.
	_, err = e.send(ctx, []solana.Instruction{inst}, rpc.CommitmentFinalized)
	return err
}

// UpdateMetadata edits the metadata via the modern Update instruction, which needs
// only the update authority, so it works after the mint authority is revoked.
func (e *Engine) UpdateMetadata(ctx context.Context, mint solana.PublicKey, name, symbol, uri string) error {
	pda, err := metadataPDA(mint)
	if err != nil {
		return fmt.Errorf("metadata PDA: %w", err)
	}
	pk := e.payer.PublicKey()
	ph := metadataProgram // program id doubles as the "none" placeholder for optional accounts
	accts := solana.AccountMetaSlice{
		solana.Meta(pk).SIGNER(),            // 0 authority (update authority)
		solana.Meta(ph),                     // 1 delegate_record (none)
		solana.Meta(ph),                     // 2 token (none)
		solana.Meta(mint),                   // 3 mint
		solana.Meta(pda).WRITE(),            // 4 metadata
		solana.Meta(ph),                     // 5 edition (none)
		solana.Meta(pk).SIGNER().WRITE(),    // 6 payer
		solana.Meta(solana.SystemProgramID), // 7 system program
		solana.Meta(sysvarInstructions),     // 8 sysvar instructions
		solana.Meta(ph),                     // 9 auth rules program (none)
		solana.Meta(ph),                     // 10 auth rules (none)
	}
	inst := solana.NewInstruction(metadataProgram, accts, updateData(name, symbol, uri))
	_, err = e.send(ctx, []solana.Instruction{inst}, rpc.CommitmentConfirmed)
	return err
}
