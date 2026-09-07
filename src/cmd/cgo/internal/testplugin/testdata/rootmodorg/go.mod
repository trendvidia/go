// The host-shared half of the rootmod fixture: a second single-element
// dotted module path with its package at the root, so its globals are
// named `example%2eorg.X`. Kept separate from example.com because the
// two halves differ in WHO links the package (see the package comments),
// and a module root holds exactly one package.
module example.org

go 1.26
