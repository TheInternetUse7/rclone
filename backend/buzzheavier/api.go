package buzzheavier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/lib/rest"
)

var retryErrorCodes = []int{429, 500, 502, 503, 504}

func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if fserrors.ContextError(ctx, &err) {
		return false, err
	}
	return fserrors.ShouldRetry(err) || fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
}

// --- API response types ---

// FsItem represents a file or folder in the BuzzHeavier filesystem.
// The API uses isDirectory (bool) to distinguish files from folders.
type FsItem struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	IsDirectory bool      `json:"isDirectory"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

// fsDirData is the inner object inside the top-level "data" field.
type fsDirData struct {
	Node       FsItem   `json:"node"`
	Children   []FsItem `json:"children"`
	NextCursor string   `json:"nextCursor"`
}

// FsDirResponse is the response from GET /api/fs or GET /api/fs/{id}.
// The API returns: {"code":200, "data": {"node":{...}, "children":[...], ...}}
type FsDirResponse struct {
	Data fsDirData `json:"data"`
}

// UploadResponse is the JSON response from a PUT upload
type UploadResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateDirRequest is the body for POST /api/fs/{parentId}
type CreateDirRequest struct {
	Name string `json:"name"`
}

// CreateDirResponse is the response from creating a directory
type CreateDirResponse struct {
	Data FsItem `json:"data"`
}

// apiError holds an error response from the BuzzHeavier API
type apiError struct {
	Message string `json:"message"`
	Status  int    `json:"status"`
}

func (e apiError) Error() string {
	return fmt.Sprintf("buzzheavier API error %d: %s", e.Status, e.Message)
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var e apiError
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(body, &e); err == nil && e.Message != "" {
		return e
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
}

// --- Directory traversal helpers ---

// getRootID returns the ID of the authenticated user's root directory.
// It reuses the listDir call: the root node ID is embedded in the response.
func (f *Fs) getRootID(ctx context.Context) (string, error) {
	var result FsDirResponse
	err := f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &rest.Opts{
			Method: "GET",
			Path:   "/fs",
		}, nil, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return "", err
	}
	return result.Data.Node.ID, nil
}

// listDir lists items in a directory by its ID.
// Returns the children slice from the API response.
func (f *Fs) listDir(ctx context.Context, dirID string) ([]FsItem, error) {
	var result FsDirResponse
	urlPath := "/fs"
	if dirID != "" {
		urlPath = "/fs/" + dirID
	}
	err := f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &rest.Opts{
			Method: "GET",
			Path:   urlPath,
		}, nil, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return nil, err
	}
	return result.Data.Children, nil
}

// dirIDForPath returns the directory ID for a path relative to f.root.
// Returns an error if the directory does not exist.
func (f *Fs) dirIDForPath(ctx context.Context, relPath string) (string, error) {
	if relPath == "" || relPath == "." {
		return f.rootID, nil
	}

	parts := strings.Split(strings.Trim(relPath, "/"), "/")
	currentID := f.rootID

	for _, part := range parts {
		items, err := f.listDir(ctx, currentID)
		if err != nil {
			return "", err
		}
		found := false
		for _, item := range items {
			if item.Name == part && item.IsDirectory {
				currentID = item.ID
				found = true
				break
			}
		}
		if !found {
			return "", fs.ErrorDirNotFound
		}
	}
	return currentID, nil
}

// findOrCreateDir resolves an absolute path (from the account root), creating
// any missing directories along the way.  It returns the directory ID.
func (f *Fs) findOrCreateDir(ctx context.Context, absPath string) (string, error) {
	// Get the account root ID
	rootID, err := f.getRootID(ctx)
	if err != nil {
		return "", fmt.Errorf("cannot get root directory ID: %w", err)
	}

	if absPath == "" || absPath == "." {
		return rootID, nil
	}

	parts := strings.Split(strings.Trim(absPath, "/"), "/")
	currentID := rootID

	for _, part := range parts {
		if part == "" {
			continue
		}
		items, err := f.listDir(ctx, currentID)
		if err != nil {
			return "", err
		}
		found := ""
		for _, item := range items {
			if item.Name == part && item.IsDirectory {
				found = item.ID
				break
			}
		}
		if found != "" {
			currentID = found
			continue
		}
		// Create missing directory
		newID, err := f.createDir(ctx, currentID, part)
		if err != nil {
			return "", fmt.Errorf("failed to create directory %q: %w", part, err)
		}
		currentID = newID
	}
	return currentID, nil
}

// createDir creates a subdirectory under parentID and returns the new dir's ID
func (f *Fs) createDir(ctx context.Context, parentID, name string) (string, error) {
	body := CreateDirRequest{Name: name}
	var result CreateDirResponse
	err := f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.CallJSON(ctx, &rest.Opts{
			Method: "POST",
			Path:   "/fs/" + parentID,
		}, &body, &result)
		return shouldRetry(ctx, resp, err)
	})
	if err != nil {
		return "", err
	}
	return result.Data.ID, nil
}

// deleteItem deletes a file or directory by ID
func (f *Fs) deleteItem(ctx context.Context, id string) error {
	return f.pacer.Call(func() (bool, error) {
		resp, err := f.srv.Call(ctx, &rest.Opts{
			Method:     "DELETE",
			RootURL:    apiBaseURL + "/fs/" + id,
			NoResponse: true,
		})
		return shouldRetry(ctx, resp, err)
	})
}

// uploadFile streams a file to the BuzzHeavier upload endpoint and returns
// the file ID from the JSON response.
func (f *Fs) uploadFile(ctx context.Context, uploadURL string, in io.Reader, size int64) (string, error) {
	var fileID string
	err := f.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, in)
		if err != nil {
			return false, err
		}
		if size >= 0 {
			req.ContentLength = size
		}
		if f.opt.AccountID != "" {
			req.Header.Set("Authorization", "Bearer "+f.opt.AccountID)
		}
		if f.opt.LocationID != "" {
			q := req.URL.Query()
			q.Set("locationId", f.opt.LocationID)
			req.URL.RawQuery = q.Encode()
		}

		client := fshttp.NewClient(ctx)
		resp, err := client.Do(req)
		if err != nil {
			return fserrors.ShouldRetry(err), err
		}
		defer func() { _ = resp.Body.Close() }()

		if err := checkResponse(resp); err != nil {
			return fserrors.ShouldRetryHTTP(resp, retryErrorCodes), err
		}

		// Parse the response to get the file ID
		// BuzzHeavier returns a streaming response; the last JSON object is
		// the upload result.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, err
		}
		// The server may return multiple JSON objects (progress + final).
		// We take the last complete JSON object.
		lastJSON := extractLastJSON(body)
		var result UploadResponse
		if err := json.Unmarshal(lastJSON, &result); err != nil {
			// Fallback: try parsing the whole body
			if err2 := json.Unmarshal(body, &result); err2 != nil {
				return false, fmt.Errorf("failed to parse upload response: %w", err)
			}
		}
		fileID = result.ID
		return false, nil
	})
	return fileID, err
}

// extractLastJSON finds the last complete JSON object in a byte slice.
// BuzzHeavier streams multiple JSON objects; we want the final one.
func extractLastJSON(data []byte) []byte {
	// Scan backwards for the last '}'
	end := -1
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '}' {
			end = i
			break
		}
	}
	if end < 0 {
		return data
	}
	// Match the opening brace
	depth := 0
	for i := end; i >= 0; i-- {
		if data[i] == '}' {
			depth++
		} else if data[i] == '{' {
			depth--
			if depth == 0 {
				return bytes.TrimSpace(data[i : end+1])
			}
		}
	}
	return data
}


