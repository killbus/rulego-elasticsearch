package main

import "github.com/rulego/rulego/api/types"

// Plugins is the symbol loaded by RuleGo's Go plugin registry. The host
// resolves this exported variable from data/plugins/*.so at boot; it is the
// single ABI entrypoint for the components this plugin contributes.
var Plugins pluginRegistry

type pluginRegistry struct{}

// Init is required by the PluginRegistry interface but is not invoked by the
// host loader; real setup happens in each node's Init.
func (*pluginRegistry) Init() error { return nil }

// Components returns the node prototypes this plugin registers. The host
// calls New() on each per chain instance.
func (*pluginRegistry) Components() []types.Node {
	return []types.Node{&elasticsearchWriteNode{}}
}
