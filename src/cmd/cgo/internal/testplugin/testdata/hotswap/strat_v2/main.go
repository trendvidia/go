package main

import "testplugin/hotswap/api"

// Version is looked up so the host can prove which image is live.
func Version() string { return "v2" }

// OnTick is deliberately cheap: this measures the swap machinery, not
// the strategy.
func OnTick(t api.Tick) api.Order {
	return api.Order{Sym: t.Sym, Qty: int64(t.Px) * 2, Note: "v2"}
}

func main() {}
