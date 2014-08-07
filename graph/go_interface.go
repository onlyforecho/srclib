package graph

import "sourcegraph.com/sourcegraph/srclib/repo"

// GoInterfaceMethod represents a Go interface method defined by an Go interface
// symbol or implemented by a Go type symbol. It is used for finding all
// implementations of an interface.
type GoInterfaceMethod struct {
	// OfSymbolPath refers to the Go interface symbol that defines this method, or the Go
	// type symbol that implements this method.
	OfSymbolPath DefPath `db:"of_symbol_path"`

	// OfSymbolUnit refers to the unit containing the symbol denoted in OfSymbolPath.
	OfSymbolUnit string `db:"of_symbol_unit"`

	// Repo refers to the repository in which this method was defined.
	Repo repo.URI

	// Key is the canonical signature of the method for the implements
	// operation. If a type's methods' keys are a superset of an interface's,
	// then the type implements the interface.
	CanonicalSignature string `db:"canonical_signature"`

	// Name is the method's name.
	Name string
}
