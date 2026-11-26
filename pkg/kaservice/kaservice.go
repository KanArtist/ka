// Package kaservice is ka's operation and query layer for programs that embed ka as a
// library (e.g. Syre's hub, which keeps a replica of a ka store and syncs it). It is the
// same Service the ka CLI, MCP server and daemon use, re-exported with a stable path.
//
// Open a workspace (a workspace file listing stores), create a Service on it, then call
// Search/Get/Create/Update/Comment/CheckItems/Transition/Claim/Release/Sync with a Caller
// identifying who acts. A Caller with Authority (e.g. "syre") takes claims that other
// ka clients treat as read-only (spec §claim.authority).
package kaservice

import (
	"context"

	"github.com/KanArtist/ka/internal/engine"
)

type (
	// Workspace is a set of stores opened together.
	Workspace = engine.Workspace
	// OpenOptions controls workspace discovery.
	OpenOptions = engine.OpenOptions
	// WorkspaceFile is the parsed form of a workspace file.
	WorkspaceFile = engine.WorkspaceFile
	// StoreSpec is one store entry of a workspace file.
	StoreSpec = engine.StoreSpec

	// Service is the operation and query layer.
	Service = engine.Service
	// Caller identifies who is acting.
	Caller = engine.Caller

	SearchIn     = engine.SearchIn
	SearchOut    = engine.SearchOut
	Summary      = engine.Summary
	Claim        = engine.Claim
	Ref          = engine.Ref
	GetIn        = engine.GetIn
	Detail       = engine.Detail
	CommentOut   = engine.CommentOut
	CreateIn     = engine.CreateIn
	CreateOut    = engine.CreateOut
	UpdateIn     = engine.UpdateIn
	WriteOut     = engine.WriteOut
	CommentIn    = engine.CommentIn
	CheckIn      = engine.CheckIn
	TransitionIn = engine.TransitionIn
	ClaimOut     = engine.ClaimOut
	NextIn       = engine.NextIn
	NextOut      = engine.NextOut
	SyncOut      = engine.SyncOut

	// Error is a structured ka error with a stable code.
	Error = engine.Error
)

// Error codes callers commonly branch on.
const (
	CodeNotFound     = engine.CodeNotFound
	CodeInvalid      = engine.CodeInvalid
	CodeClaimLost    = engine.CodeClaimLost
	CodeNotClaimed   = engine.CodeNotClaimed
	CodeNotYourClaim = engine.CodeNotYourClaim
)

// Open discovers and opens a workspace.
func Open(ctx context.Context, o OpenOptions) (*Workspace, error) { return engine.Open(ctx, o) }

// ParseWorkspaceFile reads a workspace file.
func ParseWorkspaceFile(path string) (*WorkspaceFile, error) { return engine.ParseWorkspaceFile(path) }

// New builds a Service over an opened workspace (loading its index).
func New(ctx context.Context, ws *Workspace) (*Service, error) { return engine.New(ctx, ws) }

// AsError returns err as a ka *Error, or nil.
func AsError(err error) *Error { return engine.AsError(err) }

// IsCode reports whether err is a ka error with code.
func IsCode(err error, code string) bool { return engine.IsCode(err, code) }
