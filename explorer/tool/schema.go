package main

// This file is the data contract between the tool and the app: the types
// below are written as JSON into the site's data files, and the app reads
// nothing else. Field names in JSON are the json tags; empty fields are
// left out unless the tag says otherwise.
//
// # Files and loading
//
// The site is opened from disk (file://), so data cannot be fetched. Every
// data file is a classic script that makes one call:
//
//	explorerRegister(kind, key, value)
//
// The app defines explorerRegister before it loads any data file.
//
//	kind     key                       value      file
//	"core"   ""                        Core       data/core.js (loaded first, always)
//	"chunk"  Chunk.ID                  Chunk      data/chunks/<ID>.js (File.Chunk, Package.Chunk)
//	"doc"    Document.Path             DocText    data/docs/<n>.js (Document.Chunk)
//
// Chunks and documents are loaded on demand by inserting a <script> element
// whose src is the file's path relative to index.html; the path is given in
// full by File.Chunk, Package.Chunk and Document.Chunk ("data/chunks/…").
//
// # Identity
//
// Everything is identified by the reference syntax of explorer/README.md,
// also in URLs. A symbol's identity is its Ref ("internal/box.Up",
// "internal/box.Manager.Up"), a package's its Path ("internal/box"), a
// file's its Path relative to the repository root ("internal/box/spec.go").
// The map Core.Refs resolves every reference written in the model.
//
// # Positions
//
// Lines are 1-based. Offsets inside a source text (SourceFile.Tokens and
// SourceFile.Links) are in UTF-16 code units, as JavaScript indexes strings,
// not bytes.

// Core is data/core.js: everything the app needs before the reader opens a
// file. Per-file source, "used by" lists and call edges are in chunks.
type Core struct {
	// Build describes this build of the site.
	Build Build `json:"build"`
	// Packages are the module's Go packages, sorted by Path. External test
	// packages ("internal/box_test") are separate entries with XTest set.
	Packages []Package `json:"packages"`
	// Files are every file the site shows: all Go files of the packages
	// (tests and files excluded by build constraints included) and the
	// other files a reader may want (web app, assets, examples, test data,
	// scripts). Sorted by Path.
	Files []File `json:"files"`
	// Symbols are every package-level declaration, method, struct field and
	// interface method of the module, tests included. Sorted by Ref.
	Symbols []Symbol `json:"symbols"`
	// Implementations lists which module types implement which module
	// interfaces. Interfaces without methods are left out.
	Implementations []Implementation `json:"implementations"`
	// Commands is the CLI's command tree in depth-first order, the root
	// ("devc") first.
	Commands []Command `json:"commands"`
	// RPC are the JSON-RPC methods, sorted by Name.
	RPC []RPCMethod `json:"rpc"`
	// Routes are the hub's HTTP routes in the order of the route table.
	Routes []Route `json:"routes"`
	// Events are the session event types, sorted by Name.
	Events []EventType `json:"events"`
	// Config are the configuration keys, per configuration surface in
	// declaration order (a table before its keys).
	Config []ConfigKey `json:"config"`
	// Env are the environment variables the code names, sorted by Name.
	Env []EnvVar `json:"env"`
	// Reach explains the "where code runs" data (Symbol.Actors,
	// Package.Actors).
	Reach Reach `json:"reach"`
	// Documents are the Markdown documents, sorted by Path. Their text is
	// in doc files.
	Documents []Document `json:"documents"`
	// Findings are the review findings indexed from docs/dev/reviews, in
	// review order.
	Findings []Finding `json:"findings"`
	// WorkOrders are the work orders in docs/dev/chunks, sorted by ID.
	WorkOrders []WorkOrder `json:"workOrders"`
	// Patterns are the text patterns the app turns into references inside
	// comments and documents ("finding C1-05", "work order C10a",
	// "plan 3.6", "README rule 2", "spec section 13").
	Patterns []Pattern `json:"patterns"`
	// Model is the hand-written model of explorer/model, with flow
	// connections filled in by the tool.
	Model Model `json:"model"`
	// Refs resolves every reference that occurs in the model (TOML fields
	// and [[…]] in Markdown) to what it points at.
	Refs map[string]ResolvedRef `json:"refs"`
	// Diagnostics are the problems --check reports, errors first.
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Build describes one run of the tool.
type Build struct {
	// Commit is the full hash of HEAD; empty when git is unavailable.
	Commit string `json:"commit"`
	// Dirty is set when the working tree differed from HEAD.
	Dirty bool `json:"dirty"`
	// Time is when the build started (RFC 3339, UTC).
	Time string `json:"time"`
	// Seconds is how long the build took.
	Seconds float64 `json:"seconds"`
	// GoVersion is the toolchain that type-checked the code.
	GoVersion string `json:"goVersion"`
	// Module is the Go module path ("github.com/mb706/devc").
	Module string `json:"module"`
	// Root is the repository's absolute path on the machine that will open
	// the site (for "open in editor" links and copied paths).
	Root string `json:"root"`
	// TypeErrors are the type checker's complaints, which should be none;
	// the build continues regardless and links near them may be missing.
	TypeErrors []string `json:"typeErrors,omitempty"`
	// Counts are totals for the overview.
	Counts Counts `json:"counts"`
}

// Counts are totals of one build.
type Counts struct {
	Packages  int `json:"packages"`
	GoFiles   int `json:"goFiles"`
	TestFiles int `json:"testFiles"`
	// Lines are the lines of non-test Go files; TestLines of test files.
	Lines     LineCounts `json:"lines"`
	TestLines LineCounts `json:"testLines"`
	Symbols   int        `json:"symbols"`
	Uses      int        `json:"uses"`
	Commands  int        `json:"commands"`
	RPC       int        `json:"rpc"`
	Routes    int        `json:"routes"`
	Events    int        `json:"events"`
	Config    int        `json:"config"`
	Env       int        `json:"env"`
}

// LineCounts splits a file's lines. A line with any code is code; a line
// with only comments is comment; the rest are blank.
type LineCounts struct {
	Code    int `json:"code"`
	Comment int `json:"comment"`
	Blank   int `json:"blank"`
}

// Package is one Go package of the module.
type Package struct {
	// Path is the directory relative to the repository root
	// ("internal/box"); for an external test package, the directory with
	// "_test" appended ("internal/box_test"). Reference: "pkg:<Path>".
	Path string `json:"path"`
	// ImportPath is the Go import path.
	ImportPath string `json:"importPath"`
	// Name is the package clause's name ("box", "main").
	Name string `json:"name"`
	// Doc is the package comment, as written (without comment markers).
	Doc string `json:"doc,omitempty"`
	// DocFile is the file that holds the package comment.
	DocFile string `json:"docFile,omitempty"`
	// Files are the package's files (Go files including tests and files
	// excluded by build constraints), sorted.
	Files []string `json:"files"`
	// Imports are the module packages imported by non-test files;
	// TestImports those imported only by test files. ImportedBy are the
	// module packages whose non-test files import this one. All are Paths.
	Imports     []string `json:"imports,omitempty"`
	TestImports []string `json:"testImports,omitempty"`
	ImportedBy  []string `json:"importedBy,omitempty"`
	// Lines counts the non-test Go files, TestLines the test files.
	Lines     LineCounts `json:"lines"`
	TestLines LineCounts `json:"testLines"`
	// Layer, Group and Role are copied from the package's entry in
	// packages.toml (Model.Layers, Model.Groups).
	Layer string `json:"layer,omitempty"`
	Group string `json:"group,omitempty"`
	Role  string `json:"role,omitempty"`
	// Actors maps an actor ID to the share of the package's non-test
	// function and method lines reachable from that actor's entry points
	// (0 < share <= 1; actors with no share are absent). See Reach.
	Actors map[string]float64 `json:"actors,omitempty"`
	// XTest marks an external test package; ForPackage is then the Path of
	// the package it tests.
	XTest      bool   `json:"xtest,omitempty"`
	ForPackage string `json:"forPackage,omitempty"`
	// TestOnly marks a package that has only test files
	// (test/integration).
	TestOnly bool `json:"testOnly,omitempty"`
	// Chunk is the data file with the package's sources, "used by" lists
	// and call edges.
	Chunk string `json:"chunk"`
}

// File is one file the site shows.
type File struct {
	// Path is relative to the repository root. Reference: "file:<Path>".
	Path string `json:"path"`
	// Package is the Path of the Go package the file belongs to; empty for
	// files that are not Go.
	Package string `json:"package,omitempty"`
	// Lang is the language, for highlighting: "go", "markdown", "javascript",
	// "css", "html", "shell", "toml", "json", "yaml", "dockerfile",
	// "template", "text" or "binary".
	Lang string `json:"lang"`
	// Test marks a Go test file (_test.go) and every file under a testdata
	// directory or under test/.
	Test bool `json:"test,omitempty"`
	// Ignored marks a Go file excluded by build constraints on linux/amd64
	// with the integration tag: shown, but not type-checked, so it has no
	// links and declares no symbols.
	Ignored bool `json:"ignored,omitempty"`
	// Doc is the file comment (Go files only): a comment above the package
	// clause, separated from it by a blank line, that is not a build
	// constraint. Without comment markers.
	Doc string `json:"doc,omitempty"`
	// Lines counts the file's lines (for files that are not Go: code and
	// blank only).
	Lines LineCounts `json:"lines"`
	// Bytes is the file's size.
	Bytes int `json:"bytes"`
	// Chunk is the data file holding the text; empty for binary files.
	Chunk string `json:"chunk,omitempty"`
}

// Symbol is one declaration of the module.
type Symbol struct {
	// Ref is the symbol's reference and identity: "<package>.<Name>" for
	// package-level declarations, "<package>.<Type>.<Member>" for methods,
	// struct fields and interface methods, and deeper for fields of
	// anonymous struct types ("internal/x.T.Field.Inner"). A second func
	// init of a package is "<package>.init#2", and so on.
	Ref string `json:"ref"`
	// Kind is "func", "method", "type", "var", "const", "field" or
	// "imethod" (a method declared in an interface type).
	Kind string `json:"kind"`
	// Name is the declared name.
	Name string `json:"name"`
	// Package is the Path of the declaring package.
	Package string `json:"package"`
	// Parent is, for methods, fields and interface methods, the Ref of the
	// type (or of the field whose anonymous struct type declares the
	// field).
	Parent string `json:"parent,omitempty"`
	// Exported reports whether Name is exported.
	Exported bool `json:"exported,omitempty"`
	// Test marks a declaration in a _test.go file.
	Test bool `json:"test,omitempty"`
	// TypeKind is, for types, what the type is: "struct", "interface",
	// "func", "map", "slice", "array", "chan", "pointer", "basic", "named"
	// (defined from another named type) or "alias".
	TypeKind string `json:"typeKind,omitempty"`
	// Signature is the declaration as source, without body or doc comment:
	// "func (m *Manager) Up(ctx context.Context) error", "type Spec struct",
	// "Level string `toml:\"level\"`", "const MethodSessionSend = \"session.send\"".
	// Struct and interface bodies are left out of a type's signature.
	Signature string `json:"signature"`
	// Doc is the doc comment, without comment markers. For a declaration in
	// a parenthesised group without its own comment, the group's comment;
	// for a field, a trailing line comment when there is no comment above.
	Doc string `json:"doc,omitempty"`
	// Value is a constant's value (Go syntax: strings are quoted).
	Value string `json:"value,omitempty"`
	// File, Line and EndLine locate the declaration (without its doc
	// comment).
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"endLine"`
	// Actors are the IDs of the actors whose entry points reach this
	// function or method in the call graph (see Reach). Only functions and
	// methods have actors.
	Actors []string `json:"actors,omitempty"`
	// Uses is the number of "used by" records of the symbol (Chunk.UsedBy
	// of its package's chunk).
	Uses int `json:"uses,omitempty"`
}

// Implementation records that a type implements an interface, both of the
// module.
type Implementation struct {
	// Type is the Ref of the type, Interface the Ref of the interface.
	Type      string `json:"type"`
	Interface string `json:"interface"`
	// Pointer is set when only the pointer type (*T) implements it.
	Pointer bool `json:"pointer,omitempty"`
}

// Command is one command of the CLI.
type Command struct {
	// Path is the full command ("devc box init"). Reference: "cmd:<Path>".
	Path string `json:"path"`
	// Parent is the Path of the parent command; empty for the root.
	Parent string `json:"parent,omitempty"`
	// Use is cobra's usage line ("init", "up [realm/project]").
	Use     string   `json:"use"`
	Short   string   `json:"short,omitempty"`
	Long    string   `json:"long,omitempty"`
	Example string   `json:"example,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	// Hidden commands are not listed by devc's help (most are entry points
	// of processes in a box or of helpers).
	Hidden     bool   `json:"hidden,omitempty"`
	Deprecated string `json:"deprecated,omitempty"`
	// Subcommands are the Paths of the child commands.
	Subcommands []string `json:"subcommands,omitempty"`
	// Flags are the flags defined on this command (persistent ones of
	// parents are on the parents).
	Flags []Flag `json:"flags,omitempty"`
	// Run is the Ref of the declared function the command's run function
	// belongs to: the function itself, or the function whose body holds
	// the function literal. Empty when the command has no run function.
	Run string `json:"run,omitempty"`
	// RunFile and RunLine locate the run function (the function literal
	// when RunLiteral is set).
	RunFile    string `json:"runFile,omitempty"`
	RunLine    int    `json:"runLine,omitempty"`
	RunLiteral bool   `json:"runLiteral,omitempty"`
	// Actor is the actor of system.toml whose process this command runs
	// as: the actor that names the command or its nearest parent in its
	// commands list. Empty when no actor names it or a parent.
	Actor string `json:"actor,omitempty"`
}

// Flag is one flag of a command.
type Flag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	// Type is pflag's type name ("string", "bool", "stringSlice", "count").
	Type    string `json:"type"`
	Default string `json:"default,omitempty"`
	Usage   string `json:"usage,omitempty"`
	// Persistent flags apply to the subcommands too.
	Persistent bool `json:"persistent,omitempty"`
	Hidden     bool `json:"hidden,omitempty"`
}

// Site is a place in the code where something is used.
type Site struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// In is the Ref of the enclosing declared function or declaration
	// (code in a function literal belongs to the function around it);
	// empty at package level outside any declaration with a name.
	In string `json:"in,omitempty"`
	// Handler is, for a handler registration, the Ref of the handling code:
	// the function or method registered, the function called to build the
	// handler, or In when the handler is a function literal or the site is
	// a case or a comparison.
	Handler string `json:"handler,omitempty"`
}

// RPCMethod is one JSON-RPC method: a string constant of the package a rpc
// surface names, with the surface's prefix ("Method…").
type RPCMethod struct {
	// Name is the method's wire name ("session.send"). Reference:
	// "rpc:<Name>".
	Name string `json:"name"`
	// Const is the Ref of the constant; Doc its doc comment.
	Const string `json:"const"`
	Doc   string `json:"doc,omitempty"`
	// Surfaces are the IDs of the rpc surfaces whose handlers_in functions
	// register a handler for the method or whose listed_in symbols list it.
	Surfaces []string `json:"surfaces,omitempty"`
	// Handlers are registrations: the constant as a key of a map literal or
	// of an index assignment, in a case, or compared with ==.
	Handlers []Site `json:"handlers,omitempty"`
	// Callers are sites where the constant is an argument of a call or the
	// value of a struct literal's field: code that sends the method.
	Callers []Site `json:"callers,omitempty"`
	// Listed are all other sites (slices of allowed methods, map values,
	// arguments of a call that builds a handler).
	Listed []Site `json:"listed,omitempty"`
	// Tests are the sites in test files, of any kind.
	Tests []Site `json:"tests,omitempty"`
}

// Route is one route of a http surface's route table.
type Route struct {
	// Key is "<Method> <Path>" ("POST /api/commands"). Reference:
	// "http:<Key>".
	Key    string `json:"key"`
	Method string `json:"method"`
	// Path is the pattern as the mux sees it, constant expressions
	// evaluated ("/api/sessions/{machine}/{session}").
	Path string `json:"path"`
	// Access and CrossSite are the names of the constants in the route's
	// access and cross-site columns; AccessRef and CrossSiteRef their Refs.
	Access       string `json:"access,omitempty"`
	AccessRef    string `json:"accessRef,omitempty"`
	CrossSite    string `json:"crossSite,omitempty"`
	CrossSiteRef string `json:"crossSiteRef,omitempty"`
	// Handler is the Ref of the function or method that handles the route.
	Handler string `json:"handler,omitempty"`
	// File and Line locate the route's entry in the table.
	File string `json:"file"`
	Line int    `json:"line"`
	// Surface is the ID of the http surface.
	Surface string `json:"surface"`
}

// EventType is one session event type: a string constant of the package an
// events surface names, with the surface's prefix ("Type…").
type EventType struct {
	// Name is the type as written in events ("question"). Reference:
	// "event:<Name>".
	Name  string `json:"name"`
	Const string `json:"const"`
	Doc   string `json:"doc,omitempty"`
	// Writers are sites where the constant is an argument of a call or the
	// value of a struct literal's field (code that makes such an event);
	// Readers are cases and comparisons (code that acts on one); Other are
	// the remaining non-test sites; Tests are sites in test files.
	Writers []Site `json:"writers,omitempty"`
	Readers []Site `json:"readers,omitempty"`
	Other   []Site `json:"other,omitempty"`
	Tests   []Site `json:"tests,omitempty"`
	Surface string `json:"surface"`
}

// ConfigKey is one key of a configuration file, from the toml tags of the
// configuration types.
type ConfigKey struct {
	// Key is the dotted path with the surface's key prefix first
	// ("project.network.level"). Map keys chosen by the user appear as
	// "<name>" ("realm.harness.<name>.version"). Reference:
	// "config:<Key>".
	Key string `json:"key"`
	// Parent is the Key of the enclosing table; empty for top-level keys.
	Parent string `json:"parent,omitempty"`
	// Surface is the ID of the config surface (one per file kind).
	Surface string `json:"surface"`
	// Kind is "value", "table" (a struct), "tables" (an array of tables,
	// [[key]]) or "map" (a table whose keys the user chooses).
	Kind string `json:"kind"`
	// Type is the Go type of the field, qualified by package name
	// ("string", "[]config.Hub", "map[string]config.Remote").
	Type string `json:"type"`
	// Field is the Ref of the struct field; its "used by" list says where
	// the key takes effect. Doc is the field's doc comment.
	Field string `json:"field"`
	Doc   string `json:"doc,omitempty"`
}

// EnvVar is an environment variable that the code names in a string
// literal (Go) or in the text of another file.
type EnvVar struct {
	// Name is the variable ("DEVC_BOX"). Reference: "env:<Name>".
	Name string `json:"name"`
	// Consts are the Refs of constants whose value is exactly Name; their
	// own "used by" lists are uses of the variable too.
	Consts []string `json:"consts,omitempty"`
	// Uses are the places that contain the name (In is empty outside Go).
	Uses []Site `json:"uses"`
}

// Reach explains Symbol.Actors and Package.Actors.
type Reach struct {
	// Note says how the data was computed and what it does not mean.
	Note string `json:"note"`
	// Actors are the IDs of the actors with entry points, with how many
	// functions and methods each reaches.
	Actors []ActorReach `json:"actors"`
}

// ActorReach is the size of one actor's reachable set.
type ActorReach struct {
	Actor string `json:"actor"`
	// Seeds are the Refs the search started from (command run functions
	// and entry points).
	Seeds []string `json:"seeds"`
	// Functions is the number of functions and methods reached.
	Functions int `json:"functions"`
}

// Document is one Markdown document.
type Document struct {
	// Path is relative to the repository root. Reference:
	// "doc:<Path>#<heading number or anchor>", or "doc:<Path>" for the whole.
	Path string `json:"path"`
	// Title is the text of the first heading, else the file name.
	Title string `json:"title"`
	// Group is "product" (README, spec, guides), "development" (plan,
	// status, leftover, rules), "workorder", "review", "explorer" or
	// "example".
	Group    string    `json:"group"`
	Headings []Heading `json:"headings"`
	Bytes    int       `json:"bytes"`
	// Chunk is the doc file with the text.
	Chunk string `json:"chunk"`
}

// Heading is one Markdown heading.
type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	// Number is the heading's leading section number without a trailing
	// dot ("13", "6.2"), if it has one.
	Number string `json:"number,omitempty"`
	// Anchor is the GitHub-style anchor of the heading text.
	Anchor string `json:"anchor"`
	Line   int    `json:"line"`
}

// Finding is one review finding.
type Finding struct {
	// ID is the finding's key. It is the label as written ("C1-05", "A3"),
	// except that a label that occurs in two reviews keeps the plain label
	// only in the review whose name starts with the same letter; in the
	// other it is "<review>/<label>" ("A/B1"). Reference: "finding:<ID>".
	ID    string `json:"id"`
	Label string `json:"label"`
	// Review is the review's name ("A", "C1", "F2"); File and Line locate
	// the finding in the review.
	Review string `json:"review"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	// Meta is what precedes the title: class or area, and severity
	// ("security — high", "class A, high").
	Meta  string `json:"meta,omitempty"`
	Title string `json:"title"`
	// Unconfirmed marks findings listed under the review's "Unconfirmed"
	// heading.
	Unconfirmed bool `json:"unconfirmed,omitempty"`
	// Text is the finding's Markdown, without its first line.
	Text string `json:"text"`
	// Response is the paragraph of the response file that answers the
	// finding (Markdown), from its bold lead to the next finding's;
	// ResponseFile is that file.
	Response     string `json:"response,omitempty"`
	ResponseFile string `json:"responseFile,omitempty"`
	ResponseLine int    `json:"responseLine,omitempty"`
	// Cited are the places in Go comments (tests included) that mention
	// the finding, as the patterns would link them; Site.In is the
	// declaration the comment documents or sits in.
	Cited []Site `json:"cited,omitempty"`
}

// WorkOrder is one work order of docs/dev/chunks.
type WorkOrder struct {
	// ID is the file name's first part ("C10a", "C12r1", "X1").
	// Reference: "chunk:<ID>".
	ID    string `json:"id"`
	Title string `json:"title"`
	File  string `json:"file"`
	// Cited are the places in Go comments that mention the work order (see
	// Finding.Cited).
	Cited []Site `json:"cited,omitempty"`
}

// Pattern is a text pattern that becomes references. To apply one: find the
// matches of Regex (JavaScript and Go RE2 syntax alike; apply with the
// global flag); take the text of capture group Group; if Split is set,
// every match of Split inside that text is one key, else the whole text is
// the key; the reference is Target with "{}" replaced by the key. Lookup
// says which keys count: "finding" or "chunk" only IDs of Core.Findings or
// Core.WorkOrders, "doc" only references whose document has that heading
// (by number or anchor). Apply the patterns in order, and skip a match that
// overlaps an earlier match that produced a reference. Without Split the
// whole match becomes the link ("plan 3.6"); with Split each key does.
type Pattern struct {
	Name   string `json:"name"`
	Regex  string `json:"regex"`
	Group  int    `json:"group"`
	Split  string `json:"split,omitempty"`
	Target string `json:"target"`
	Lookup string `json:"lookup,omitempty"`
}

// ResolvedRef is what a reference of the model points at.
type ResolvedRef struct {
	// Kind is "symbol", "package", "file", "command", "rpc", "route",
	// "event", "config", "env", "doc", "finding", "chunk", or the kind of a
	// model object: "concern", "flow", "term", "actor", "zone", "channel",
	// "diagram", "surface", "layer", "group".
	Kind string `json:"kind"`
	// Key is the target's identity in its table: a Symbol's Ref, a
	// Package's or File's Path, a Command's Path, an RPCMethod's Name, a
	// Route's Key, an EventType's Name, a ConfigKey's Key, an EnvVar's
	// Name, a Document's Path, a Finding's or WorkOrder's ID, or a model
	// object's ID (a diagram's "<concern>/<id>").
	Key string `json:"key"`
	// File and Line locate the target in the code where that makes sense
	// (symbols, files with "~text", commands, constants of methods and
	// events, routes, config fields).
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
	// Anchor is the heading anchor of a "doc:" reference.
	Anchor string `json:"anchor,omitempty"`
}

// Diagnostic is one problem found by the checks.
type Diagnostic struct {
	// Severity is "error" or "warning".
	Severity string `json:"severity"`
	// File is the model file or code file concerned; Place says where in
	// it ("step 4 ref", "[[actor]] daemon"); Message says what is wrong.
	File    string `json:"file"`
	Place   string `json:"place,omitempty"`
	Message string `json:"message"`
}

// Chunk is one data file loaded on demand: the sources of one Go package,
// or the files that are not Go of one directory, with the "used by" lists
// and call edges of the package's symbols.
type Chunk struct {
	// ID is the file name without ".js": the directory with "/" replaced
	// by "~" ("internal~box"), "go~" before it for packages and "dir~" for
	// other files ("go~internal~box", "dir~web~app", "dir~." for the root).
	ID string `json:"id"`
	// Package is the Path of the Go package; empty for other files.
	Package string       `json:"package,omitempty"`
	Files   []SourceFile `json:"files"`
	// UsedBy maps the Ref of each symbol of the package to the places that
	// use it, in file and line order (definitions excluded).
	UsedBy map[string][]Use `json:"usedBy,omitempty"`
	// Calls maps the Ref of each function, method or other declaration of
	// the package to the functions and methods it calls or mentions.
	Calls map[string][]Call `json:"calls,omitempty"`
	// DynamicCallers maps the Ref of each method of the package to the
	// declarations that may call it through a module interface (static
	// callers are in UsedBy).
	DynamicCallers map[string][]Call `json:"dynamicCallers,omitempty"`
}

// SourceFile is the text of one file with its annotations.
type SourceFile struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	// Tokens are triples (start, length, class) for highlighting, where
	// start is the offset from the previous triple's start (the first from
	// 0) and class is 1 keyword, 2 string or character, 3 comment, 4
	// number. Go files only; for other files the app highlights by itself.
	Tokens []int `json:"tokens,omitempty"`
	// Links are triples (start, length, target) for identifiers that refer
	// to the module: start is relative to the previous triple's start like
	// in Tokens, target an index into Targets. Definitions are linked to
	// themselves.
	Links []int `json:"links,omitempty"`
	// Targets are the references the links point to: symbol Refs, and
	// "pkg:<Path>" for package names of imports.
	Targets []string `json:"targets,omitempty"`
}

// Use is one place that uses a symbol.
type Use struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// In is the Ref of the enclosing declaration (see Site.In).
	In string `json:"in,omitempty"`
	// Kind is "call" (called), "type" (in a type expression), "field" (a
	// key of a struct literal) or "ref" (any other mention, including a
	// function used as a value).
	Kind string `json:"kind"`
	// Test marks a use in a test file.
	Test bool `json:"test,omitempty"`
	// Text is the line of code, trimmed, at most 160 characters.
	Text string `json:"text"`
}

// Call is one edge of the call graph, seen from one end.
type Call struct {
	// Ref is the other end: the callee in Chunk.Calls, the caller in
	// Chunk.DynamicCallers.
	Ref string `json:"ref"`
	// Line is the line of the first such call or mention in the caller.
	Line int `json:"line"`
	// Dynamic marks an edge through a module interface; Via is then the
	// Ref of the interface method called.
	Dynamic bool   `json:"dynamic,omitempty"`
	Via     string `json:"via,omitempty"`
}

// DocText is the text of one document (a doc file).
type DocText struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// ----- The model (explorer/model). Field docs are in explorer/README.md;
// the toml tags are the format, the json tags what the app reads.

// Model is the content of explorer/model.
type Model struct {
	// Overview is overview.md (Markdown).
	Overview string    `json:"overview"`
	Zones    []Zone    `json:"zones"`
	Actors   []Actor   `json:"actors"`
	Channels []Channel `json:"channels"`
	// Layers are in packages.toml's order, bottom to top.
	Layers []Layer `json:"layers"`
	Groups []Group `json:"groups"`
	// Concerns are sorted by Order, then ID.
	Concerns []Concern `json:"concerns"`
	// Flows are sorted by ID.
	Flows    []Flow    `json:"flows"`
	Terms    []Term    `json:"terms"`
	Trees    []Tree    `json:"trees"`
	Mounts   []Mount   `json:"mounts"`
	Surfaces []Surface `json:"surfaces"`
}

// Zone is a place where code runs (system.toml [[zone]]).
type Zone struct {
	ID    string `toml:"id" json:"id"`
	Title string `toml:"title" json:"title"`
	// Trust is "untrusted", "host", "aux", "hub", "device" or "external".
	Trust   string `toml:"trust" json:"trust"`
	Summary string `toml:"summary" json:"summary"`
	// Col, Row, ColSpan and RowSpan place the zone on the runtime map's
	// grid (1-based; spans default to 1).
	Col     int `toml:"col" json:"col"`
	Row     int `toml:"row" json:"row"`
	ColSpan int `toml:"colspan" json:"colspan"`
	RowSpan int `toml:"rowspan" json:"rowspan"`
}

// Actor is a process, store, person or service (system.toml [[actor]]).
type Actor struct {
	ID    string `toml:"id" json:"id"`
	Zone  string `toml:"zone" json:"zone"`
	Title string `toml:"title" json:"title"`
	// Kind is "process", "store", "person" or "service".
	Kind    string `toml:"kind" json:"kind"`
	Summary string `toml:"summary" json:"summary"`
	// Commands are the command paths ("devc box init") that run as this
	// actor; each stands for itself and its subcommands, except those
	// another actor names (Command.Actor has the result). Entry are other
	// entry points (symbol references). An actor with either is devc code.
	Commands []string `toml:"commands" json:"commands,omitempty"`
	Entry    []string `toml:"entry" json:"entry,omitempty"`
}

// Channel connects two actors (system.toml [[channel]]).
type Channel struct {
	ID        string `toml:"id" json:"id"`
	From      string `toml:"from" json:"from"`
	To        string `toml:"to" json:"to"`
	Title     string `toml:"title" json:"title"`
	Transport string `toml:"transport" json:"transport"`
	Carries   string `toml:"carries" json:"carries"`
	// Boundary marks a channel that crosses a trust boundary.
	Boundary bool `toml:"boundary" json:"boundary,omitempty"`
	// Checks are references to where what arrives is validated.
	Checks   []string `toml:"checks" json:"checks,omitempty"`
	Concerns []string `toml:"concerns" json:"concerns,omitempty"`
}

// Layer is one layer of packages.toml, with its ID from the layers list.
type Layer struct {
	ID      string `toml:"-" json:"id"`
	Title   string `toml:"title" json:"title"`
	Summary string `toml:"summary" json:"summary"`
}

// Group is one [[group]] of packages.toml.
type Group struct {
	ID      string `toml:"id" json:"id"`
	Title   string `toml:"title" json:"title"`
	Summary string `toml:"summary" json:"summary,omitempty"`
}

// Concern is one concerns/<id>.md: front matter and Markdown body.
type Concern struct {
	ID      string `toml:"id" json:"id"`
	Title   string `toml:"title" json:"title"`
	Summary string `toml:"summary" json:"summary"`
	// Order sorts the concerns (reading order).
	Order    int      `toml:"order" json:"order"`
	Packages []string `toml:"packages" json:"packages,omitempty"`
	Files    []string `toml:"files" json:"files,omitempty"`
	Key      []string `toml:"key" json:"key,omitempty"`
	Spec     []string `toml:"spec" json:"spec,omitempty"`
	Tests    []string `toml:"tests" json:"tests,omitempty"`
	Related  []string `toml:"related" json:"related,omitempty"`
	// Invariants and Diagrams are [[invariant]] and [[diagram]].
	Invariants []Invariant `toml:"invariant" json:"invariants,omitempty"`
	Diagrams   []Diagram   `toml:"diagram" json:"diagrams,omitempty"`
	// Body is the Markdown after the front matter; File the model file.
	Body string `toml:"-" json:"body"`
	File string `toml:"-" json:"file"`
}

// Invariant is a property that must hold, with where it is enforced.
type Invariant struct {
	Text string `toml:"text" json:"text"`
	// Refs are where it is enforced; Tests the tests that show it.
	Refs  []string `toml:"refs" json:"refs,omitempty"`
	Tests []string `toml:"tests" json:"tests,omitempty"`
}

// Diagram is a concern's own diagram.
type Diagram struct {
	ID    string `toml:"id" json:"id"`
	Title string `toml:"title" json:"title"`
	// Kind is "graph": boxes on a grid and arrows.
	Kind  string        `toml:"kind" json:"kind"`
	Nodes []DiagramNode `toml:"node" json:"nodes"`
	Edges []DiagramEdge `toml:"edge" json:"edges"`
}

// DiagramNode is a box of a diagram.
type DiagramNode struct {
	ID    string `toml:"id" json:"id"`
	Label string `toml:"label" json:"label"`
	// Col and Row place the node (1-based).
	Col int    `toml:"col" json:"col"`
	Row int    `toml:"row" json:"row"`
	Ref string `toml:"ref" json:"ref,omitempty"`
	// Zone colours the node like that zone.
	Zone string `toml:"zone" json:"zone,omitempty"`
}

// DiagramEdge is an arrow of a diagram.
type DiagramEdge struct {
	From  string `toml:"from" json:"from"`
	To    string `toml:"to" json:"to"`
	Label string `toml:"label" json:"label,omitempty"`
	Ref   string `toml:"ref" json:"ref,omitempty"`
}

// Flow is one flows/<id>.toml.
type Flow struct {
	ID string `toml:"id" json:"id"`
	// Kind is "flow" (a sequence across actors) or "tour" (a reading
	// order).
	Kind    string `toml:"kind" json:"kind"`
	Title   string `toml:"title" json:"title"`
	Summary string `toml:"summary" json:"summary"`
	Concern string `toml:"concern" json:"concern,omitempty"`
	Trigger string `toml:"trigger" json:"trigger,omitempty"`
	// Actors are the lifelines, in order.
	Actors []string `toml:"actors" json:"actors,omitempty"`
	Steps  []Step   `toml:"step" json:"steps"`
	File   string   `toml:"-" json:"file"`
}

// Step is one [[step]] of a flow. Exactly one of At (an actor does
// something), From and To (something passes between two actors) and
// Section (a heading) is set; in a tour, a step may also have only a Ref.
type Step struct {
	At      string `toml:"at" json:"at,omitempty"`
	From    string `toml:"from" json:"from,omitempty"`
	To      string `toml:"to" json:"to,omitempty"`
	Section string `toml:"section" json:"section,omitempty"`
	// Via is how a message passes: "exec", "rpc:<method>", "http:<route>",
	// "event:<type>", "cmd:<command>", "reply", "file", "signal" or
	// "stream".
	Via   string `toml:"via" json:"via,omitempty"`
	Label string `toml:"label" json:"label,omitempty"`
	// Ref is the step's code: what the actor runs, or what sends.
	Ref string `toml:"ref" json:"ref,omitempty"`
	// External marks a step outside devc's code (no Ref).
	External bool     `toml:"external" json:"external,omitempty"`
	Text     string   `toml:"text" json:"text,omitempty"`
	Detail   string   `toml:"detail" json:"detail,omitempty"`
	Checks   []string `toml:"checks" json:"checks,omitempty"`
	// Unchecked says why the connection to this step cannot be seen by
	// the tool; the tool then does not look for one.
	Unchecked string `toml:"unchecked" json:"unchecked,omitempty"`
	// Connections are filled in by the tool: how this step connects to the
	// steps before it, one entry per actor of the step that has an earlier
	// step with code (for a message, the receiver's entry is on the
	// receiver's next step).
	Connections []Connection `toml:"-" json:"connections,omitempty"`
	// Sender is filled in by the tool for a message whose Via names an RPC
	// method or an event type: how the step's Ref is linked to the code
	// that sends it (Kind "rpc" or "event"; Path from Ref to the calling
	// site's function, or from that function to Ref).
	Sender *Connection `toml:"-" json:"sender,omitempty"`
}

// Connection is how a step connects to an earlier one of the same actor.
type Connection struct {
	// Actor is the actor whose consecutive steps are connected.
	Actor string `json:"actor"`
	// From is the index (in Flow.Steps) of the earlier step.
	From int `json:"from"`
	// Status is "found" (the tool found the connection), "unchecked" (the
	// step's Unchecked gives the reason) or "missing" (none found: a
	// warning of --check).
	Status string `json:"status"`
	// Kind is how it was found: "same" (the same code), "path" (a path in
	// the call graph), "rpc", "route", "event" or "command" (through that
	// table), or "reply" (the answer to a call returns to its caller).
	Kind string `json:"kind,omitempty"`
	// Path is the chain of Refs found in the call graph, from the earlier
	// code to the later (for "rpc", "route", "event" and "command": from
	// the handler or run function to the step's code). Edges that go
	// through an interface are marked in DynamicEdges by the index of
	// their target in Path.
	Path         []string `json:"path,omitempty"`
	DynamicEdges []int    `json:"dynamicEdges,omitempty"`
	// Via is the reference of the table entry that links the two (an
	// "rpc:", "http:", "event:" or "cmd:" reference).
	Via string `json:"via,omitempty"`
	// Note explains the result in a sentence.
	Note string `json:"note,omitempty"`
}

// Term is one glossary.toml [[term]].
type Term struct {
	ID    string   `toml:"id" json:"id"`
	Title string   `toml:"title" json:"title"`
	Aka   []string `toml:"aka" json:"aka,omitempty"`
	Text  string   `toml:"text" json:"text"`
	Refs  []string `toml:"refs" json:"refs,omitempty"`
	// See are IDs of related terms.
	See []string `toml:"see" json:"see,omitempty"`
}

// Tree is a directory tree of layout.toml.
type Tree struct {
	ID      string      `toml:"id" json:"id"`
	Title   string      `toml:"title" json:"title"`
	Root    string      `toml:"root" json:"root"`
	Summary string      `toml:"summary" json:"summary"`
	Entries []TreeEntry `toml:"entry" json:"entries"`
}

// TreeEntry is one path in a tree.
type TreeEntry struct {
	// Path is relative to the tree's root; "<name>" marks a part that
	// varies.
	Path string `toml:"path" json:"path"`
	Text string `toml:"text" json:"text"`
	// Writer is "host", "box", "both" or "user".
	Writer string `toml:"writer" json:"writer,omitempty"`
	Ref    string `toml:"ref" json:"ref,omitempty"`
}

// Mount is one mount of a box (layout.toml [[mount]]).
type Mount struct {
	Source string `toml:"source" json:"source"`
	Target string `toml:"target" json:"target"`
	// Mode is "rw", "ro" or another short note ("tmpfs").
	Mode string `toml:"mode" json:"mode"`
	Text string `toml:"text" json:"text"`
	Ref  string `toml:"ref" json:"ref,omitempty"`
}

// Surface is one table of entry points (interfaces.toml [[surface]]). The
// fields after Summary tell the tool where to find the table.
type Surface struct {
	ID string `toml:"id" json:"id"`
	// Kind is "rpc", "http", "commands", "events", "config" or "env".
	Kind    string `toml:"kind" json:"kind"`
	Title   string `toml:"title" json:"title"`
	Summary string `toml:"summary" json:"summary"`
	// HandlersIn (rpc) are the functions in which the surface's handlers
	// are registered; ListedIn are functions or variables that list the
	// surface's methods (an allowlist from which handlers are built).
	HandlersIn []string `toml:"handlers_in" json:"handlersIn,omitempty"`
	ListedIn   []string `toml:"listed_in" json:"listedIn,omitempty"`
	// Consts and Prefix (rpc, events) name the package ("pkg:…") whose
	// string constants with that name prefix are the methods or types.
	Consts string `toml:"consts" json:"consts,omitempty"`
	Prefix string `toml:"prefix" json:"prefix,omitempty"`
	// Table (http) is the function that returns the route table, a slice
	// literal of structs; Fields maps the roles "method", "path",
	// "access", "xsite" and "handler" to the struct's field names.
	Table  string            `toml:"table" json:"table,omitempty"`
	Fields map[string]string `toml:"fields" json:"fields,omitempty"`
	// Root (config) is the struct type of the file; Key the first part of
	// its keys ("project").
	Root string `toml:"root" json:"root,omitempty"`
	Key  string `toml:"key" json:"key,omitempty"`
	// Pattern (env) is the regular expression of the variables' names.
	Pattern string `toml:"pattern" json:"pattern,omitempty"`
}
