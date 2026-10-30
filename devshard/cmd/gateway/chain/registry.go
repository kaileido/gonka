package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"

	"devshard/signing"
)

const registryMemberTupleLength = 2

// RegistryInstance is one entry of the gateway-registry contract, in the contract's JSON.
type RegistryInstance struct {
	InstanceID    string   `json:"instance_id"`
	Endpoint      string   `json:"endpoint"`
	SPKISHA256    string   `json:"spki_sha256"`
	Protocol      uint16   `json:"protocol"`
	Topics        []string `json:"topics"`
	UpdatedHeight uint64   `json:"updated_height"`
}

// RegistryMember is one address and its unexpired instances.
type RegistryMember struct {
	Address   string
	Instances []RegistryInstance
}

// RegistryRegistration is what one instance writes about itself.
type RegistryRegistration struct {
	InstanceID string   `json:"instance_id"`
	Endpoint   string   `json:"endpoint"`
	SPKISHA256 string   `json:"spki_sha256"`
	Protocol   uint16   `json:"protocol"`
	Topics     []string `json:"topics"`
}

type membersOfQuery struct {
	MembersOf membersOfArguments `json:"members_of"`
}

type membersOfArguments struct {
	Addresses []string `json:"addresses"`
}

type registerInstanceExecute struct {
	RegisterInstance RegistryRegistration `json:"register_instance"`
}

// MembersOf runs the contract's MembersOf smart query; the contract takes at most 100 addresses per call.
func (g *GRPCChain) MembersOf(ctx context.Context, contract string, addresses []string) ([]RegistryMember, error) {
	query, err := json.Marshal(membersOfQuery{MembersOf: membersOfArguments{Addresses: addresses}})
	if err != nil {
		return nil, fmt.Errorf("encode members_of query: %w", err)
	}
	response, err := wasmtypes.NewQueryClient(g.conn()).SmartContractState(ctx, &wasmtypes.QuerySmartContractStateRequest{Address: contract, QueryData: query})
	if err != nil {
		return nil, fmt.Errorf("query registry contract: %w", err)
	}
	return decodeRegistryMembers(response.Data)
}

func decodeRegistryMembers(data []byte) ([]RegistryMember, error) {
	var tuples [][]json.RawMessage
	if err := json.Unmarshal(data, &tuples); err != nil {
		return nil, fmt.Errorf("decode members_of reply: %w", err)
	}
	members := make([]RegistryMember, 0, len(tuples))
	for _, tuple := range tuples {
		if len(tuple) != registryMemberTupleLength {
			return nil, fmt.Errorf("decode members_of reply: tuple of %d elements", len(tuple))
		}
		var member RegistryMember
		if err := json.Unmarshal(tuple[0], &member.Address); err != nil {
			return nil, fmt.Errorf("decode members_of address: %w", err)
		}
		if err := json.Unmarshal(tuple[1], &member.Instances); err != nil {
			return nil, fmt.Errorf("decode members_of instances: %w", err)
		}
		members = append(members, member)
	}
	return members, nil
}

// RegisterInstance broadcasts the contract's RegisterInstance as an unordered tx signed by the allowlisted key, and waits for it to commit.
func (c *TxClient) RegisterInstance(ctx context.Context, signer *signing.Secp256k1Signer, contract string, registration RegistryRegistration) error {
	if signer == nil {
		return fmt.Errorf("signer is required")
	}
	chainID, err := c.resolveChainID(ctx)
	if err != nil {
		return err
	}
	account, err := c.transport.Account(ctx, signer.Address())
	if err != nil {
		return err
	}
	execute, err := json.Marshal(registerInstanceExecute{RegisterInstance: registration})
	if err != nil {
		return fmt.Errorf("encode register_instance: %w", err)
	}
	txBytes, err := buildExecuteContractTx(signer, chainID, account.Number, c.feeDenom, c.feeAmount, c.gasLimit, contract, execute, c.now().Add(UnorderedTxTTL))
	if err != nil {
		return err
	}
	txHash := txHashFromBytes(txBytes)
	nodeHash, err := c.transport.Broadcast(ctx, txBytes)
	if err != nil {
		return err
	}
	if !strings.EqualFold(nodeHash, txHash) {
		return fmt.Errorf("tx hash mismatch: precomputed %s, node returned %s", txHash, nodeHash)
	}
	if err := c.waitForCommit(ctx, txHash); err != nil {
		return fmt.Errorf("awaiting registration commit for tx %s: %w", txHash, err)
	}
	return nil
}
