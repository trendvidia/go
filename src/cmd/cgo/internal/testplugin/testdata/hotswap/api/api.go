// Package api holds the types crossing the host/strategy boundary.
//
// Every type here is defined by the HOST and compiled into both sides.
// That is the first rule of the upgrade contract in plugin.Close: a
// strategy may not hand the host a type the strategy defines, or the
// host keeps a reference into an image it is about to unload.
package api

// Tick is one market update.
type Tick struct {
	Sym string
	Px  float64
	Seq int64
}

// Order is what a strategy decides to do about a Tick.
type Order struct {
	Sym  string
	Qty  int64
	Note string
}

// OnTick is the whole strategy surface. A func value obtained through
// plugin.Lookup is the only thing the host holds from a strategy, which
// is the second rule: reach the plugin only through Lookup.
type OnTick func(Tick) Order
