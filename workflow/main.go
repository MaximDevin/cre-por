//go:build wasip1

// Package main is the deployable Chainlink CRE workflow: a CRON-triggered
// Proof-of-Reserve check whose HTTP traffic runs through the CRE HTTP
// capability (never net/http directly, which the sandbox does not allow).
//
// Build (requires the WASI target, as for every CRE workflow):
//
//	GOOS=wasip1 GOARCH=wasm go build -o por.wasm ./workflow
//
// Registration: `cre` CLI + project.yaml/workflow.yaml (see README §8).
// The local, runnable simulation of this exact callback lives in
// cmd/por-run; this file is the on-network artifact.
package main

import (
	"github.com/smartcontractkit/cre-sdk-go/cre"
	"github.com/smartcontractkit/cre-sdk-go/cre/wasm"
)

func main() {
	wasm.NewRunner(cre.ParseJSON[Config]).Run(InitWorkflow)
}
