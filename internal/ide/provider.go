package ide

import (
	"encoding/json"
	"fmt"
	"os"
)

// StateProvider supplies the editor-state answers for the 12 IDE MCP tools.
// The default HeadlessProvider treats the local filesystem as ground truth and
// auto-accepts diffs; richer providers (e.g. a real editor bridge) can replace it.
type StateProvider interface {
	OpenDiff(oldPath, newPath, newContents, tabName string) (ToolCallResult, error)
	GetDiagnostics(uri string) (ToolCallResult, error)
	GetOpenEditors() (ToolCallResult, error)
	GetWorkspaceFolders() (ToolCallResult, error)
	GetCurrentSelection() (ToolCallResult, error)
	GetLatestSelection() (ToolCallResult, error)
	OpenFile(args OpenFileArgs) (ToolCallResult, error)
	CloseTab(tabName string) (ToolCallResult, error)
	CloseAllDiffTabs() (ToolCallResult, error)
	CheckDocumentDirty(filePath string) (ToolCallResult, error)
	SaveDocument(filePath string) (ToolCallResult, error)
	ExecuteCode(code string) (ToolCallResult, error)
}

type OpenFileArgs struct {
	FilePath          string `json:"filePath"`
	Preview           bool   `json:"preview"`
	StartText         string `json:"startText"`
	EndText           string `json:"endText"`
	SelectToEndOfLine bool   `json:"selectToEndOfLine"`
	MakeFrontmost     bool   `json:"makeFrontmost"`
}

// HeadlessProvider answers as a no-UI editor backed by the real filesystem.
// Decisions match the chosen architecture: openDiff writes new_file_contents to
// disk and auto-accepts (returns FILE_SAVED); selection/openEditors are empty.
type HeadlessProvider struct {
	WorkspaceFolders []string
}

func NewHeadlessProvider(workspaceFolders []string) *HeadlessProvider {
	return &HeadlessProvider{WorkspaceFolders: workspaceFolders}
}

func jsonText(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func (h *HeadlessProvider) OpenDiff(oldPath, newPath, newContents, tabName string) (ToolCallResult, error) {
	// Headless: auto-accept. Persist the proposed contents to the real file and
	// report FILE_SAVED + the saved text, exactly the shape the extension's
	// accepted path returns ([{text:"FILE_SAVED"},{text:<contents>}]).
	target := newPath
	if target == "" {
		target = oldPath
	}
	if target == "" {
		return ToolCallResult{}, fmt.Errorf("openDiff: no file path provided")
	}
	if err := os.WriteFile(target, []byte(newContents), 0o644); err != nil {
		return ToolCallResult{}, err
	}
	return TextResult("FILE_SAVED", newContents), nil
}

func (h *HeadlessProvider) GetDiagnostics(uri string) (ToolCallResult, error) {
	// No language server in headless mode: empty diagnostics array.
	return TextResult(jsonText([]any{})), nil
}

func (h *HeadlessProvider) GetOpenEditors() (ToolCallResult, error) {
	return TextResult(jsonText(map[string]any{"tabs": []any{}})), nil
}

func (h *HeadlessProvider) GetWorkspaceFolders() (ToolCallResult, error) {
	folders := make([]map[string]any, 0, len(h.WorkspaceFolders))
	for _, f := range h.WorkspaceFolders {
		folders = append(folders, map[string]any{"name": f, "uri": "file://" + f, "path": f})
	}
	return TextResult(jsonText(map[string]any{"folders": folders})), nil
}

func (h *HeadlessProvider) GetCurrentSelection() (ToolCallResult, error) {
	return TextResult(jsonText(map[string]any{"success": false, "message": "No selection available"})), nil
}

func (h *HeadlessProvider) GetLatestSelection() (ToolCallResult, error) {
	return TextResult(jsonText(map[string]any{"success": false, "message": "No selection available"})), nil
}

func (h *HeadlessProvider) OpenFile(args OpenFileArgs) (ToolCallResult, error) {
	if _, err := os.Stat(args.FilePath); err != nil {
		return ToolCallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: err.Error()}}}, nil
	}
	return TextResult(fmt.Sprintf("Opened file: %s", args.FilePath)), nil
}

func (h *HeadlessProvider) CloseTab(tabName string) (ToolCallResult, error) {
	return TextResult("TAB_CLOSED"), nil
}

func (h *HeadlessProvider) CloseAllDiffTabs() (ToolCallResult, error) {
	return TextResult("CLOSED_0_DIFF_TABS"), nil
}

func (h *HeadlessProvider) CheckDocumentDirty(filePath string) (ToolCallResult, error) {
	// Headless has no unsaved buffers; nothing is ever dirty.
	return TextResult(jsonText(map[string]any{"isDirty": false, "filePath": filePath})), nil
}

func (h *HeadlessProvider) SaveDocument(filePath string) (ToolCallResult, error) {
	return TextResult(jsonText(map[string]any{"saved": true, "filePath": filePath})), nil
}

func (h *HeadlessProvider) ExecuteCode(code string) (ToolCallResult, error) {
	return ToolCallResult{
		IsError: true,
		Content: []ContentItem{{Type: "text", Text: "executeCode is not supported in headless mode (no Jupyter kernel)"}},
	}, nil
}
