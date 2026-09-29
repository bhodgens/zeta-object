// actions_hook.go — the action-subsystem seam. The bucket-actions engine
// (package main actions.go) stays main-side; the moved handlers reach it
// through this hook so the frontend never imports package main. main()
// installs the concrete implementation via installActionTrigger.
package s3

// ActionContext carries the action-execution context. Field-for-field the
// package-main struct (moved so the handler call sites keep their shape);
// the wiring adapter converts to the main-side type in one place.
type ActionContext struct {
	FilePath     string
	MetadataPath string
	BucketName   string
	BucketPath   string
	ObjectKey    string
	ContentType  string
	ETag         string
	Size         int64
}

// actionTriggerFn is the installed after_upload/after_download/after_delete
// trigger. Nil = actions disabled (unit tests).
var actionTriggerFn func(eventType string, ctx ActionContext)

// installActionTrigger installs the action trigger. package main calls
// this at wiring time with a wrapper that adapts ActionContext to its own
// identical-shaped struct.
func installActionTrigger(fn func(eventType string, ctx ActionContext)) {
	actionTriggerFn = fn
}

// triggerActions fires the installed action trigger (no-op when none).
func triggerActions(eventType string, ctx ActionContext) {
	if actionTriggerFn != nil {
		actionTriggerFn(eventType, ctx)
	}
}
