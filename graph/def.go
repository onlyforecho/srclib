package graph

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jmoiron/sqlx/types"
	"sourcegraph.com/sourcegraph/srclib/repo"
)

type (
	SID        int64
	DefPath    string
	SymbolKind string
	TreePath   string
)

// START DefKey OMIT
// DefKey specifies a definition, either concretely or abstractly. A concrete
// definition key has a non-empty CommitID and refers to a definition defined in a
// specific commit. An abstract definition key has an empty CommitID and is
// considered to refer to definitions from any number of commits (so long as the
// Repo, UnitType, Unit, and Path match).
//
// You can think of CommitID as the time dimension. With an empty CommitID, you
// are referring to a definition that may or may not exist at various times. With a
// non-empty CommitID, you are referring to a specific definition of a definition at
// the time specified by the CommitID.
type DefKey struct {
	// Repo is the VCS repository that defines this definition. Its Elasticsearch mapping is defined
	// separately.
	Repo repo.URI `json:",omitempty"`

	// CommitID is the ID of the VCS commit that this definition was defined in. The
	// CommitID is always a full commit ID (40 hexadecimal characters for git
	// and hg), never a branch or tag name.
	CommitID string `db:"commit_id" json:",omitempty"`

	// UnitType is the type name of the source unit (obtained from unit.Type(u))
	// that this definition was defined in.
	UnitType string `db:"unit_type" json:",omitempty"`

	// Unit is the name of the source unit (obtained from u.Name()) that this
	// definition was defined in.
	Unit string `json:",omitempty"`

	// Path is a unique identifier for the symbol, relative to the source unit.
	// It should remain stable across commits as long as the symbol is the
	// "same" symbol. Its Elasticsearch mapping is defined separately (because
	// it is a multi_field, which the struct tag can't currently represent).
	//
	// Path encodes no structural semantics. Its only meaning is to be a stable
	// unique identifier within a given source unit. In many languages, it is
	// convenient to use the namespace hierarchy (with some modifications) as
	// the Path, but this may not always be the case. I.e., don't rely on Path
	// to find parents or children or any other structural propreties of the
	// symbol hierarchy). See Symbol.TreePath instead.
	Path DefPath
}

// END DefKey OMIT

func (s DefKey) String() string {
	b, err := json.Marshal(s)
	if err != nil {
		panic("SymbolKey.String: " + err.Error())
	}
	return string(b)
}

// START Def OMIT
type Def struct {
	// SID is a unique, sequential ID for a symbol. It is regenerated each time
	// the symbol is emitted by the grapher and saved to the database. The SID
	// is used as an optimization (e.g., joins are faster on SID than on
	// DefKey).
	SID SID `db:"sid" json:",omitempty" elastic:"type:integer,index:no"`

	// DefKey is the natural unique key for a symbol. It is stable
	// (subsequent runs of a grapher will emit the same symbols with the same
	// DefKeys).
	DefKey

	// TreePath is a structurally significant path descriptor for a symbol. For
	// many languages, it may be identical or similar to DefKey.Path.
	// However, it has the following constraints, which allow it to define a
	// symbol tree.
	//
	// A tree-path is a chain of '/'-delimited components. A component is either a
	// symbol name or a ghost component.
	// - A symbol name satifies the regex [^/-][^/]*
	// - A ghost component satisfies the regex -[^/]*
	// Any prefix of a tree-path that terminates in a symbol name must be a valid
	// tree-path for some symbol.
	// The following regex captures the children of a tree-path X: X(/-[^/]*)*(/[^/-][^/]*)
	TreePath TreePath `db:"treepath" json:",omitempty"`

	// Kind is the language-independent kind of this symbol.
	Kind SymbolKind `elastic:"type:string,index:analyzed"`

	Name string

	// Callable is true if this symbol may be called or invoked, such as in the
	// case of functions or methods.
	Callable bool `db:"callable"`

	File string `elastic:"type:string,index:no"`

	DefStart int `db:"def_start" elastic:"type:integer,index:no"`
	DefEnd   int `db:"def_end" elastic:"type:integer,index:no"`

	Exported bool `elastic:"type:boolean,index:not_analyzed"`

	// Test is whether this symbol is defined in test code (as opposed to main
	// code). For example, definitions in Go *_test.go files have Test = true.
	Test bool `elastic:"type:boolean,index:not_analyzed" json:",omitempty"`

	// Data contains additional language- and toolchain-specific information
	// about the symbol. Data is used to construct function signatures,
	// import/require statements, language-specific type descriptions, etc.
	Data types.JsonText `json:",omitempty" elastic:"type:object,enabled:false"`
}

// END Def OMIT

var treePathRegexp = regexp.MustCompile(`^(?:[^/]+)(?:/[^/]+)*$`)

func (p TreePath) IsValid() bool {
	return treePathRegexp.MatchString(string(p))
}

func (s *Def) Fmt() SymbolPrintFormatter { return PrintFormatter(s) }

// HasImplementations returns true if this symbol is a Go interface and false
// otherwise. It is used for the interface implementations queries. TODO(sqs):
// run this through the golang toolchain instead of doing it here.
func (s *Def) HasImplementations() bool {
	if s.Kind != Type {
		return false
	}
	var m map[string]string
	json.Unmarshal(s.Data, &m)
	return m["Kind"] == "interface"
}

func (s *Def) sortKey() string { return s.DefKey.String() }

// Propagate describes type/value propagation in code. A Propagate entry from A
// (src) to B (dst) indicates that the type/value of A propagates to B. In Tern,
// this is indicated by A having a "fwd" property whose value is an array that
// includes B.
//
//
// ## Motivation & example
//
// For example, consider the following JavaScript code:
//
//   var a = Foo;
//   var b = a;
//
// Foo, a, and b are each their own symbol. We could resolve all of them to the
// symbol of their original type (perhaps Foo), but there are occasions when you
// do want to see only the definition of a or b and examples thereof. Therefore,
// we need to represent them as distinct symbols.
//
// Even though Foo, a, and b are distinct symbols, there are propagation
// relationships between them that are important to represent. The type of Foo
// propagates to both a and b, and the type of a propagates to b. In this case,
// we would have 3 Propagates: Propagate{Src: "Foo", Dst: "a"}, Propagate{Src:
// "Foo", Dst: "b"}, and Propagate{Src: "a", Dst: "b"}. (The propagation
// relationships could be described by just the first and last Propagates, but
// we explicitly include all paths as a denormalization optimization to avoid
// requiring an unbounded number of DB queries to determine which symbols a type
// propagates to or from.)
//
//
// ## Directionality
//
// Propagation is unidirectional, in the general case. In the example above, if
// Foo referred to a JavaScript object and if the code were evaluated, any
// *runtime* type changes (e.g., setting a property) on Foo, a, and b would be
// reflected on all of the others. But this doesn't hold for static analysis;
// it's not always true that if a property "a.x" or "b.x" exists, then "Foo.x"
// exists. The simplest example is when Foo is an external definition. Perhaps
// this example file (which uses Foo as a library) modifies Foo to add a new
// property, but other libraries that use Foo would never see that property
// because they wouldn't be executed in the same context as this example file.
// So, in general, we cannot say that Foo receives all types applied to symbols
// that Foo propagates to.
//
//
// ## Hypothetical Python example
//
// Consider the following 2 Python files:
//
//   """file1.py"""
//   class Foo(object): end
//
//   """file2.py"""
//   from .file1 import Foo
//   Foo2 = Foo
//
// In this example, there would be one Propagate: Propagate{Src: "file1/Foo",
// Dst: "file2/Foo2}.
type Propagate struct {
	// Src is the symbol whose type/value is being propagated to the dst symbol.
	SrcRepo     repo.URI
	SrcPath     DefPath
	SrcUnit     string
	SrcUnitType string

	// Dst is the symbol that is receiving a propagated type/value from the src symbol.
	DstRepo     repo.URI
	DstPath     DefPath
	DstUnit     string
	DstUnitType string
}

const (
	Const   SymbolKind = "const"
	Field              = "field"
	Func               = "func"
	Module             = "module"
	Package            = "package"
	Type               = "type"
	Var                = "var"
)

var AllSymbolKinds = []SymbolKind{Const, Field, Func, Module, Package, Type, Var}

func IsContainer(symbolKind SymbolKind) bool {
	switch symbolKind {
	case Module:
		fallthrough
	case Package:
		fallthrough
	case Type:
		return true
	default:
		return false
	}
}

// Returns true iff k is a known symbol kind.
func (k SymbolKind) Valid() bool {
	for _, kk := range AllSymbolKinds {
		if k == kk {
			return true
		}
	}
	return false
}

// SQL

func (x SID) Value() (driver.Value, error) {
	return int64(x), nil
}

func (x *SID) Scan(v interface{}) error {
	if data, ok := v.(int64); ok {
		*x = SID(data)
		return nil
	}
	return fmt.Errorf("%T.Scan failed: %v", x, v)
}

func (x DefPath) Value() (driver.Value, error) {
	return string(x), nil
}

func (x *DefPath) Scan(v interface{}) error {
	if data, ok := v.([]byte); ok {
		*x = DefPath(data)
		return nil
	}
	return fmt.Errorf("%T.Scan failed: %v", x, v)
}

func (x TreePath) Value() (driver.Value, error) {
	return string(x), nil
}

func (x *TreePath) Scan(v interface{}) error {
	if data, ok := v.([]byte); ok {
		*x = TreePath(data)
		return nil
	}
	return fmt.Errorf("%T.Scan failed: %v", x, v)
}

func (x SymbolKind) Value() (driver.Value, error) {
	return string(x), nil
}

func (x *SymbolKind) Scan(v interface{}) error {
	if data, ok := v.([]byte); ok {
		*x = SymbolKind(data)
		return nil
	}
	return fmt.Errorf("%T.Scan failed: %v", x, v)
}

// Debugging

func (x Def) String() string {
	s, _ := json.Marshal(x)
	return string(s)
}

// Sorting

type Defs []*Def

func (vs Defs) Len() int           { return len(vs) }
func (vs Defs) Swap(i, j int)      { vs[i], vs[j] = vs[j], vs[i] }
func (vs Defs) Less(i, j int) bool { return vs[i].sortKey() < vs[j].sortKey() }

func (syms Defs) Keys() (keys []DefKey) {
	keys = make([]DefKey, len(syms))
	for i, sym := range syms {
		keys[i] = sym.DefKey
	}
	return
}

func (syms Defs) SIDs() (ids []SID) {
	ids = make([]SID, len(syms))
	for i, sym := range syms {
		ids[i] = sym.SID
	}
	return
}

func ParseSIDs(sidstrs []string) (sids []SID) {
	sids = make([]SID, len(sidstrs))
	for i, sidstr := range sidstrs {
		sid, err := strconv.Atoi(sidstr)
		if err == nil {
			sids[i] = SID(sid)
		}
	}
	return
}
