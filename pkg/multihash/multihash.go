/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

// Package multihash contains functions for converting between multihashes and CIDs.
package multihash

import (
	"fmt"

	gocid "github.com/ipfs/go-cid"
	"github.com/multiformats/go-multibase"
	mh "github.com/multiformats/go-multihash"
)

// ToV1CID takes a multibase-encoded multihash and converts it to a V1 CID.
func ToV1CID(multibaseEncodedMultihash string) (string, error) {
	_, multihashBytes, err := multibase.Decode(multibaseEncodedMultihash)
	if err != nil {
		return "", fmt.Errorf("failed to decode multibase-encoded multihash: %w", err)
	}

	_, multihash, err := mh.MHFromBytes(multihashBytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse the decoded multibase value as a multihash: %w", err)
	}

	return gocid.NewCidV1(gocid.Raw, multihash).String(), nil
}

// CIDToMultihash takes a V0 or V1 CID and converts it to a multibase-encoded (with base64url as the base) multihash.
func CIDToMultihash(cid string) (string, error) {
	parsedCID, err := gocid.Decode(cid)
	if err != nil {
		return "", fmt.Errorf("failed to decode CID: %w", err)
	}

	multihashFromCID := parsedCID.Hash()

	multibaseEncodedMultihash, err := multibase.Encode(multibase.Base64url, multihashFromCID)
	if err != nil {
		return "", fmt.Errorf("failed to encoded multihash as a multibase-encoded string: %w", err)
	}

	return multibaseEncodedMultihash, nil
}
