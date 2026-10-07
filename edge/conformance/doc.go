// Package conformance checks earth-edge's filter against the code it stands
// in front of: CometBFT's RPC argument decoding (cometbft_test.go), and
// grpc-gateway's route matching and each request's PageRequest over every
// route the chain's and the SDK's protos annotate (gateway_test.go,
// pagination_test.go). The chain is the commit chain.pin names, fetched by
// fetch-chain.sh into .chain/; TestVersionsMatchChain holds this module's
// CometBFT and grpc-gateway to the versions that chain requires. A separate
// module so the proxy itself depends on nothing but the standard library.
package conformance
