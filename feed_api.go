package hydrocarbon

import (
	"context"
	"errors"
	"net/http"

	"github.com/fortytw2/hydrocarbon/discollect"
)

const maxFailedResolutions = 8

// A FeedStore is an interface used to seperate the FeedAPI from knowledge of the
// actual underlying database
type FeedStore interface {
	AddFeed(ctx context.Context, sessionKey, folderID, title, plugin, feedURL string, initConf *discollect.Config) (string, error)
	CheckIfFeedExists(ctx context.Context, sessionKey, folderID, plugin, url string) (*Feed, bool, error)
	RemoveFeed(ctx context.Context, sessionKey, folderID, feedID string) error

	AddFolder(ctx context.Context, sessionKey, name string) (string, error)

	// GetFolders should not return any Posts in the nested Feeds
	GetFolders(ctx context.Context, sessionKey string) ([]*Folder, error)
	GetFeedsForFolder(ctx context.Context, sessionKey string, folderID string, limit, offset int) ([]*Feed, error)
	GetFeed(ctx context.Context, sessionKey, feedID string, limit, offset int) (*Feed, error)
}

// FeedAPI encapsulates everything related to user management
type FeedAPI struct {
	s  FeedStore
	ks *KeySigner
	dc *discollect.Discollector
}

// NewFeedAPI returns a new Feed API
func NewFeedAPI(s FeedStore, dc *discollect.Discollector, ks *KeySigner) *FeedAPI {
	return &FeedAPI{
		s:  s,
		ks: ks,
		dc: dc,
	}
}

// AddFeed adds the specified feed to the given user
// if folder_id is left out, the feed is added to the users "default" folder
func (fa *FeedAPI) AddFeed(w http.ResponseWriter, r *http.Request) error {
	key, err := fa.ks.Verify(r.Header.Get("X-Hydrocarbon-Key"))
	if err != nil {
		return err
	}

	var feed struct {
		FolderID string `json:"folder_id,omitempty"`
		URL      string `json:"url"`
	}

	err = limitDecoder(r, &feed)
	if err != nil {
		return err
	}

	if feed.URL == "" {
		return errors.New("one of url or plugin is empty")
	}

	var blacklist []string
	var feedTitle string
	var id string

	for {
		plugin, handlerOpts, err := fa.dc.PluginForEntrypoint(feed.URL, blacklist)
		if err != nil {
			return err
		}

		// check if the plugin exists
		dbFeed, ok, err := fa.s.CheckIfFeedExists(r.Context(), key, feed.FolderID, plugin.Name, feed.URL)
		if err != nil {
			return err
		}

		if ok {
			return writeSuccess(w, map[string]string{
				"id":    dbFeed.ID,
				"title": dbFeed.Title,
			})
		}

		var initialConfig *discollect.Config
		feedTitle, initialConfig, err = plugin.ConfigCreator(feed.URL, handlerOpts)
		if err != nil {
			if len(blacklist) == maxFailedResolutions {
				return err
			}
			blacklist = append(blacklist, plugin.Name)
			continue
		}

		id, err = fa.s.AddFeed(r.Context(), key, feed.FolderID, feedTitle, plugin.Name, feed.URL, initialConfig)
		if err != nil {
			return err
		}

		break
	}

	return writeSuccess(w, map[string]string{
		"id":    id,
		"title": feedTitle,
	})
}

// AddFolder creates a new folder
func (fa *FeedAPI) AddFolder(w http.ResponseWriter, r *http.Request) error {
	key, err := fa.ks.Verify(r.Header.Get("X-Hydrocarbon-Key"))
	if err != nil {
		return err
	}

	var folder struct {
		Name string `json:"name"`
	}

	err = limitDecoder(r, &folder)
	if err != nil {
		return err
	}

	id, err := fa.s.AddFolder(r.Context(), key, folder.Name)
	if err != nil {
		return err
	}

	return writeSuccess(w, map[string]string{
		"id": id,
	})
}

// RemoveFeed removes the given feed from the users list
func (fa *FeedAPI) RemoveFeed(w http.ResponseWriter, r *http.Request) error {
	key, err := fa.ks.Verify(r.Header.Get("X-Hydrocarbon-Key"))
	if err != nil {
		return err
	}

	var feed struct {
		FolderID string `json:"folder_id"`
		FeedID   string `json:"feed_id"`
	}

	err = limitDecoder(r, &feed)
	if err != nil {
		return err
	}

	if feed.FeedID == "" || feed.FolderID == "" {
		return errors.New("no feed or folder ID sent")
	}

	return fa.s.RemoveFeed(r.Context(), key, feed.FolderID, feed.FeedID)
}

// GetFolders writes all of a users folders out
func (fa *FeedAPI) GetFolders(w http.ResponseWriter, r *http.Request) error {
	key, err := fa.ks.Verify(r.Header.Get("X-Hydrocarbon-Key"))
	if err != nil {
		return err
	}

	folders, err := fa.s.GetFolders(r.Context(), key)
	if err != nil {
		return err
	}

	return writeSuccess(w, folders)
}

// GetFeedsForFolder writes a specific feed
func (fa *FeedAPI) GetFeedsForFolder(w http.ResponseWriter, r *http.Request) error {
	key, err := fa.ks.Verify(r.Header.Get("X-Hydrocarbon-Key"))
	if err != nil {
		return err
	}

	var id struct {
		FolderID string `json:"folder_id"`
	}

	err = limitDecoder(r, &id)
	if err != nil {
		return err
	}

	feed, err := fa.s.GetFeedsForFolder(r.Context(), key, id.FolderID, 50, 0)
	if err != nil {
		return err
	}

	return writeSuccess(w, feed)
}

// GetFeed writes a specific feed
func (fa *FeedAPI) GetFeed(w http.ResponseWriter, r *http.Request) error {
	key, err := fa.ks.Verify(r.Header.Get("X-Hydrocarbon-Key"))
	if err != nil {
		return err
	}

	var id struct {
		FeedID string `json:"feed_id"`
	}

	err = limitDecoder(r, &id)
	if err != nil {
		return err
	}

	feed, err := fa.s.GetFeed(r.Context(), key, id.FeedID, 50, 0)
	if err != nil {
		return err
	}

	return writeSuccess(w, feed)
}
