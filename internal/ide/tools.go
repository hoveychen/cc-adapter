package ide

// toolDefinitions returns the 12 IDE tools the real VS Code extension exposes,
// with input schemas mirroring the reverse-engineered zod definitions in
// extension.js. The real claude binary discovers these via tools/list.
func toolDefinitions() []Tool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}

	return []Tool{
		{
			Name:        "openDiff",
			Description: "Open a git diff for the file",
			InputSchema: obj(map[string]any{
				"old_file_path":     str("Path to the file to show diff for. If not provided, uses active editor."),
				"new_file_path":     str("Path to the file to show diff for. If not provided, uses active editor."),
				"new_file_contents": str("Contents of the new file. If not provided then the current file contents of new_file_path will be used."),
				"tab_name":          str("Path to the file to show diff for. If not provided, uses active editor."),
			}),
		},
		{
			Name:        "getDiagnostics",
			Description: "Get language diagnostics from VS Code",
			InputSchema: obj(map[string]any{
				"uri": str("Optional file URI to get diagnostics for. If not provided, gets diagnostics for all files."),
			}),
		},
		{
			Name:        "getOpenEditors",
			Description: "Get information about currently open editors",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "getWorkspaceFolders",
			Description: "Get all workspace folders currently open in the IDE",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "getCurrentSelection",
			Description: "Get the current text selection in the active editor",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "getLatestSelection",
			Description: "Get the most recent text selection (even if not in the active editor)",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "openFile",
			Description: "Open a file in the editor and optionally select a range of text",
			InputSchema: obj(map[string]any{
				"filePath":          str("Path to the file to open"),
				"preview":           map[string]any{"type": "boolean", "description": "Whether to open the file in preview mode", "default": false},
				"startText":         str("Text pattern to find the start of the selection range. Selects from the beginning of this match."),
				"endText":           str("Text pattern to find the end of the selection range. Selects up to the end of this match. If not provided, only the startText match will be selected."),
				"selectToEndOfLine": map[string]any{"type": "boolean", "description": "Whether to extend the selection to the end of the line"},
				"makeFrontmost":     map[string]any{"type": "boolean", "description": "Whether to make the file the frontmost editor", "default": true},
			}, "filePath"),
		},
		{
			Name:        "close_tab",
			Description: "",
			InputSchema: obj(map[string]any{"tab_name": str("")}, "tab_name"),
		},
		{
			Name:        "closeAllDiffTabs",
			Description: "Close all diff tabs in the editor",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "checkDocumentDirty",
			Description: "Check if a document has unsaved changes (is dirty)",
			InputSchema: obj(map[string]any{"filePath": str("Path to the file to check")}, "filePath"),
		},
		{
			Name:        "saveDocument",
			Description: "Save a document with unsaved changes",
			InputSchema: obj(map[string]any{"filePath": str("Path to the file to save")}, "filePath"),
		},
		{
			Name:        "executeCode",
			Description: "Execute python code in the Jupyter kernel for the current notebook file.",
			InputSchema: obj(map[string]any{"code": str("The code to be executed on the kernel.")}, "code"),
		},
	}
}
