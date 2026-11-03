// Command qntx-otari-plugin is the QNTX LLM provider backed by otari.
package main

import (
	plugingrpc "github.com/teranos/QNTX/plugin/grpc"
	otariplugin "github.com/teranos/otari-qntx/plugin"
)

func main() {
	plugingrpc.Run(otariplugin.New(), 50140)
}
