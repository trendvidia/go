// A module path that is a single dotted element, with its package at
// the module ROOT. objabi.PathToPrefix escapes every dot AFTER the
// final slash, so when the whole import path IS the last element every
// dot is escaped and no slash survives into the symbol name:
//
//	module example.com  ->  example%2ecom.Counter
//
// That is the shape trendvidia/go#110 is about — isUserPackageName's
// "no slash means stdlib" arm rejected it, so the global never reached
// userDataExports and each module kept its own instance on Windows.
//
// go 1.26 matches the main module's go.mod (written by testMain); a
// replacement module may not require a NEWER language version than the
// main module. See gofakemod/go.mod for what that error looks like.
module example.com

go 1.26
