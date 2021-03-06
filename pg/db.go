package pg

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/fortytw2/hydrocarbon"
	"github.com/fortytw2/hydrocarbon/discollect"
)

// A DB is responsible for all interactions with postgres
type DB struct {
	sql *sql.DB
}

// NewDB returns a new database
func NewDB(dsn string, autoExplain bool) (*DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	err = runMigrations(db)
	if err != nil {
		return nil, err
	}

	if autoExplain {
		_, err = db.Exec(`LOAD 'auto_explain';`)
		if err != nil {
			return nil, err
		}

		_, err = db.Exec(`SET auto_explain.log_min_duration = 0;`)
		if err != nil {
			return nil, err
		}
	}

	return &DB{
		sql: db,
	}, nil
}

// CreateOrGetUser creates a new user and returns the users ID
func (db *DB) CreateOrGetUser(ctx context.Context, email string) (string, bool, error) {
	row := db.sql.QueryRowContext(ctx, `
	INSERT INTO users 
	(email) 
	VALUES ($1)
	ON CONFLICT (email)
	DO UPDATE SET email = EXCLUDED.email
	RETURNING id, stripe_subscription_id;`, email)

	var userID string
	var stripeSubID sql.NullString
	err := row.Scan(&userID, &stripeSubID)
	if err != nil {
		return "", false, err
	}

	return userID, stripeSubID.Valid, nil
}

// SetStripeIDs sets a users stripe IDs
func (db *DB) SetStripeIDs(ctx context.Context, userID, customerID, subID string) error {
	_, err := db.sql.ExecContext(ctx, `
	UPDATE users 
	SET (stripe_customer_id, stripe_subscription_id) = ($1, $2)
	WHERE id = $3;`, customerID, subID, userID)

	return err
}

// CreateLoginToken creates a new one-time-use login token
func (db *DB) CreateLoginToken(ctx context.Context, userID, userAgent, ip string) (string, error) {
	row := db.sql.QueryRowContext(ctx, `
	INSERT INTO login_tokens
	(user_id, user_agent, ip)
	VALUES ($1, $2, $3::cidr)
	RETURNING token;`, userID, userAgent, ip)

	var token string
	err := row.Scan(&token)
	if err != nil {
		return "", err
	}

	return token, nil
}

// VerifyKey checks that the session exists in the database
func (db *DB) VerifyKey(ctx context.Context, key string) error {
	row := db.sql.QueryRowContext(ctx, `
	SELECT id 
	FROM sessions 
	WHERE key = $1 AND active = TRUE`, key)

	var id string
	err := row.Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return errors.New("invalid or inactive token")
		}
		return err
	}

	return nil
}

// ActivateLoginToken activates the given LoginToken and returns the user
// the token was for
func (db *DB) ActivateLoginToken(ctx context.Context, token string) (string, error) {
	row := db.sql.QueryRowContext(ctx, `
	UPDATE login_tokens
	SET used = true
	WHERE token = $1
	AND expires_at > now()
	AND used = false
	RETURNING user_id;`, token)

	var userID string
	err := row.Scan(&userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", errors.New("token invalid")
		}
		return "", err
	}

	return userID, nil
}

// CreateSession creates a new session for the user ID and returns the
// session key
func (db *DB) CreateSession(ctx context.Context, userID, userAgent, ip string) (email string, key string, err error) {
	row := db.sql.QueryRowContext(ctx, `
	INSERT INTO sessions 
	(user_id, user_agent, ip)
	VALUES ($1, $2, $3::cidr)
	RETURNING key;`, userID, userAgent, ip)
	err = row.Scan(&key)
	if err != nil {
		return "", "", err
	}

	row = db.sql.QueryRowContext(ctx, `
	SELECT email
	FROM users
	WHERE id = $1`, userID)
	err = row.Scan(&email)
	if err != nil {
		return "", "", err
	}

	return email, key, nil
}

// ListSessions lists all sessions a user has
func (db *DB) ListSessions(ctx context.Context, key string, page int) ([]*hydrocarbon.Session, error) {
	rows, err := db.sql.QueryContext(ctx, `
	SELECT created_at, user_agent, ip, active
	FROM sessions
	WHERE user_id = (SELECT user_id FROM sessions WHERE key = $1)
	LIMIT 25
	OFFSET $2`, key, page)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*hydrocarbon.Session
	for rows.Next() {
		var s hydrocarbon.Session
		err = rows.Scan(&s.CreatedAt, &s.UserAgent, &s.IP, &s.Active)
		if err != nil {
			return nil, err
		}
		out = append(out, &s)
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DeactivateSession invalidates the current session
func (db *DB) DeactivateSession(ctx context.Context, key string) error {
	_, err := db.sql.QueryContext(ctx, `
	UPDATE sessions
	SET (active) = (false)
	WHERE key = $1;`, key)

	return err
}

// AddFeed adds the given URL to the users default folder
// and links it across feed_folder
func (db *DB) AddFeed(ctx context.Context, sessionKey, folderID, title, plugin, feedURL string, initialConfig *discollect.Config) (string, error) {
	if folderID == "" {
		// ensure we don't shadow folderID
		var err error
		folderID, err = db.getDefaultFolderID(ctx, sessionKey)
		if err != nil {
			return "", err
		}
	}

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}

	row := tx.QueryRowContext(ctx, `
	INSERT INTO feeds
	(title, plugin, url)
	VALUES ($1, $2, $3)
	RETURNING id;`, title, plugin, feedURL)

	var feedID uuid.UUID
	err = row.Scan(&feedID)
	if err != nil {
		txErr := tx.Rollback()
		if txErr != nil {
			return "", fmt.Errorf("%s - %s", err, txErr)
		}
		return "", err
	}

	_, err = tx.ExecContext(ctx, `
	INSERT INTO feed_folders
	(user_id, folder_id, feed_id)
	VALUES
	((SELECT user_id FROM sessions WHERE key = $1), $2, $3);`, sessionKey, folderID, feedID)
	if err != nil {
		txErr := tx.Rollback()
		if txErr != nil {
			return "", fmt.Errorf("%s - %s", err, txErr)
		}
		return "", err
	}

	_, err = tx.ExecContext(ctx, `
	INSERT INTO scrapes
	(feed_id, plugin, config)
	VALUES 
	($1, $2, $3)`, feedID, plugin, initialConfig)
	if err != nil {
		txErr := tx.Rollback()
		if txErr != nil {
			return "", fmt.Errorf("%s - %s", err, txErr)
		}
		return "", err
	}

	return feedID.String(), tx.Commit()
}

// CheckIfFeedExists checks if a given feed exists in the DB already, and if it
// does, adds it to the folder specified
func (db *DB) CheckIfFeedExists(ctx context.Context, sessionKey, folderID, plugin, url string) (*hydrocarbon.Feed, bool, error) {
	row := db.sql.QueryRowContext(ctx, `
	SELECT id, title FROM feeds WHERE url = $1 and plugin = $2`, url, plugin)

	var id uuid.UUID
	var title string
	err := row.Scan(&id, &title)
	// if the row does not exist move on
	if err != nil {
		if err != sql.ErrNoRows {
			return nil, false, err
		}
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
	}

	_, err = db.sql.ExecContext(ctx, `
	INSERT INTO feed_folders
	(user_id, folder_id, feed_id)
	VALUES
	((SELECT user_id FROM sessions WHERE key = $1), $2, $3);`, sessionKey, folderID, id)
	if err != nil {
		return nil, false, err
	}

	return &hydrocarbon.Feed{
		ID:    id.String(),
		Title: title,
	}, true, nil
}

// getDefaultFolderID returns a users default folder ID
func (db *DB) getDefaultFolderID(ctx context.Context, sessionKey string) (string, error) {
	row := db.sql.QueryRowContext(ctx, `
	SELECT id FROM folders 
	WHERE name = 'default' 
	AND user_id = (SELECT user_id FROM sessions WHERE key = $1);`, sessionKey)

	var fid string
	err := row.Scan(&fid)
	if err != nil {
		// if there is no default folder, go create one
		if err == sql.ErrNoRows {
			row := db.sql.QueryRowContext(ctx, `
			INSERT INTO folders
			(user_id)
			VALUES 
			((SELECT user_id FROM sessions WHERE key = $1 LIMIT 1))
			RETURNING id;`, sessionKey)

			err = row.Scan(&fid)
			if err != nil {
				return "", fmt.Errorf("could not create default folder: %s", err)
			}

		} else {
			return "", fmt.Errorf("could not find default folder: %s", err)
		}

	}

	return fid, nil
}

// AddFolder creates a new folder
func (db *DB) AddFolder(ctx context.Context, sessionKey, name string) (string, error) {
	row := db.sql.QueryRow(`
	INSERT INTO folders 
	(user_id, name) 
	VALUES 
	((SELECT user_id FROM sessions WHERE key = $1), $2)
	RETURNING id;`, sessionKey, name)

	var id string
	err := row.Scan(&id)
	if err != nil {
		return "", err
	}

	return id, nil
}

// RemoveFeed removes the given feed ID from the user
func (db *DB) RemoveFeed(ctx context.Context, sessionKey, folderID, feedID string) error {
	_, err := db.sql.ExecContext(ctx, `
	DELETE FROM feed_folders 
	WHERE user_id = (SELECT user_id FROM sessions WHERE key = $1 LIMIT 1)
	AND folder_id = $2
	AND feed_id = $3;`, sessionKey, folderID, feedID)

	return err
}

// GetFolders returns all of the folders for a user - if there are none it creates a
// default folder
func (db *DB) GetFoldersWithFeeds(ctx context.Context, sessionKey string) ([]*hydrocarbon.Folder, error) {
	rows, err := db.sql.QueryContext(ctx, `
	SELECT fo.name as folder_name, fo.id as folder_id, jsonb_agg(
		json_build_object('id', f.id, 'title', f.title)
	) as feeds
	FROM folders fo
	LEFT JOIN feed_folders ff ON (fo.user_id = ff.user_id AND fo.id = ff.folder_id)
	LEFT JOIN feeds f ON (ff.feed_id = f.id)
	WHERE fo.user_id = (SELECT user_id FROM sessions WHERE key = $1 LIMIT 1) 
	GROUP BY fo.name, fo.id
	ORDER BY fo.name DESC;`, sessionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	folders := make([]*hydrocarbon.Folder, 0)
	for rows.Next() {
		var folderName, folderID string
		var feedJSON []byte

		err = rows.Scan(&folderName, &folderID, &feedJSON)
		if err != nil {
			return nil, err
		}

		var feeds []*hydrocarbon.Feed
		err := json.Unmarshal(feedJSON, &feeds)
		if err != nil {
			return nil, err
		}

		folders = append(folders, &hydrocarbon.Folder{
			ID:    folderID,
			Title: folderName,
			Feeds: feeds,
		})
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return folders, nil
}

// GetFeedPosts returns a single feed
func (db *DB) GetFeedPosts(ctx context.Context, sessionKey, feedID string, limit, offset int) (*hydrocarbon.Feed, error) {
	rows, err := db.sql.QueryContext(ctx, `
	SELECT po.id, po.title, po.author, po.url, po.posted_at, (EXISTS(SELECT 1 FROM read_statuses WHERE post_id = po.id AND user_id = (SELECT user_id FROM sessions WHERE key = $1)))
	FROM posts po
	WHERE po.feed_id = $2
	AND EXISTS (SELECT 1 FROM sessions WHERE key = $1)
	ORDER BY po.posted_at DESC
	LIMIT $3 OFFSET $4`, sessionKey, feedID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	feed := &hydrocarbon.Feed{
		ID:    feedID,
		Posts: make([]*hydrocarbon.Post, 0),
	}

	for rows.Next() {
		var id, title, author, url string
		var postedAt time.Time
		var read bool

		err := rows.Scan(&id, &title, &author, &url, &postedAt, &read)
		if err != nil {
			return nil, err
		}

		feed.Posts = append(feed.Posts, &hydrocarbon.Post{
			ID:          id,
			Title:       title,
			Author:      author,
			OriginalURL: url,
			PostedAt:    postedAt,
			Read:        read,
		})
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return feed, nil
}

func (db *DB) GetPost(ctx context.Context, sessionKey, postID string) (*hydrocarbon.Post, error) {
	row := db.sql.QueryRowContext(ctx, `
	SELECT po.id, po.title, po.body, po.author, po.url, po.posted_at, (EXISTS(SELECT 1 FROM read_statuses WHERE post_id = po.id AND user_id = (SELECT user_id FROM sessions WHERE key = $1)))
	FROM posts po WHERE id = $2
	AND EXISTS (SELECT id FROM sessions WHERE key = $1);`, sessionKey, postID)

	var id uuid.UUID
	var title, author, url string
	var postedAt time.Time
	var read bool
	var compressedBody string
	err := row.Scan(&id, &title, &compressedBody, &author, &url, &postedAt, &read)
	if err != nil {
		return nil, err
	}

	body, err := decompressText(compressedBody)
	if err != nil {
		return nil, err
	}

	return &hydrocarbon.Post{
		ID:          id.String(),
		PostedAt:    postedAt,
		Title:       title,
		Body:        body,
		Author:      author,
		OriginalURL: url,
		Read:        read,
	}, nil
}

func (db *DB) MarkRead(ctx context.Context, sessionKey, postID string) error {
	_, err := db.sql.ExecContext(ctx, `
	INSERT INTO read_statuses
	(user_id, post_id)
	VALUES 
	((SELECT user_id FROM sessions WHERE key = $1), $2)
	ON CONFLICT DO NOTHING`, sessionKey, postID)
	return err
}

// Write saves off the post to the db
func (db *DB) Write(ctx context.Context, scrapeID uuid.UUID, f interface{}) error {
	hcp, ok := f.(*hydrocarbon.Post)
	if !ok {
		return errors.New("unable to write non *hydrocarbon.Post struct")
	}

	contentHash := hcp.ContentHash()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil
	}

	rollback := true
	// defer rollback if we throw an error
	defer func() {
		if rollback {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil {
				err = fmt.Errorf("err: %s, rollbackErr: %s", err, rollbackErr)
			}
		}
	}()

	var validHash string
	err = tx.QueryRowContext(ctx, `
		SELECT content_hash FROM posts WHERE content_hash = $1`, contentHash).Scan(&validHash)
	if err != nil {
		if err != sql.ErrNoRows {
			return err
		}
	}

	// do no work
	if validHash != "" {
		return nil
	}

	body, err := compressText(hcp.Body)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO posts 
		(feed_id, content_hash, title, author, body, url, posted_at)
		VALUES 
		((SELECT feed_id FROM scrapes WHERE id = $1), $2, $3, $4, $5, $6, $7)
		ON CONFLICT (url) DO UPDATE SET title = EXCLUDED.title, author = EXCLUDED.author, body = EXCLUDED.body, content_hash = EXCLUDED.content_hash;`,
		scrapeID, hcp.ContentHash(), hcp.Title, hcp.Author, body, hcp.OriginalURL, hcp.PostedAt)
	if err != nil {
		return err
	}

	rollback = false
	err = tx.Commit()
	return err
}

// Close implements io.Closer for pg.DB
func (db *DB) Close() error {
	return nil
}

// StartScrapes selects a subset of scrapes that should currently be running, but
// are not yet.
func (db *DB) StartScrapes(ctx context.Context, limit int) (ss []*discollect.Scrape, err error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := true
	// defer rollback if we throw an error
	defer func() {
		if rollback {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil {
				err = fmt.Errorf("err: %s, rollbackErr: %s", err, rollbackErr)
			}
		}
	}()

	// FOR UPDATE SKIP LOCKED allows us to reduce contention against
	// any other instance running this same query at the same time.
	rows, err := tx.QueryContext(ctx, `
	SELECT id 
	FROM scrapes
	WHERE scheduled_start_at <= now()
	AND state = 'WAITING'
	AND cardinality(errors) < 3
	LIMIT $1
	FOR UPDATE SKIP LOCKED;`, limit)
	if err != nil {
		return nil, err
	}

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		err = rows.Scan(&id)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	// return an empty array
	if len(ids) == 0 {
		return ss, nil
	}

	rows, err = tx.QueryContext(ctx, `
	UPDATE scrapes 
	SET state = 'RUNNING', started_at = now() 
	WHERE id = ANY($1)
	RETURNING id, feed_id, plugin, config;`, pq.Array(ids))
	if err != nil {
		return nil, err
	}

	for rows.Next() {
		var s discollect.Scrape
		err = rows.Scan(&s.ID, &s.FeedID, &s.Plugin, &s.Config)
		if err != nil {
			return nil, err
		}
		ss = append(ss, &s)
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	rollback = false
	err = tx.Commit()
	if err != nil {
		return nil, err
	}

	return ss, nil
}

// ListScrapes is used to list and filter scrapes, for both session resumption
// and UI purposes
func (db *DB) ListScrapes(ctx context.Context, stateFilter string, limit, offset int) ([]*discollect.Scrape, error) {
	rows, err := db.sql.QueryContext(ctx, `
	SELECT id, feed_id, plugin, config, created_at, scheduled_start_at, 
		started_at, ended_at, state, errors, 
		total_datums, total_retries, total_tasks
	FROM scrapes
	WHERE state = $1::scrape_state LIMIT $2 OFFSET $3`, stateFilter, limit, offset)
	if err != nil {
		return nil, err
	}

	var rsArr []*discollect.Scrape
	for rows.Next() {
		var rs discollect.Scrape
		err := rows.Scan(&rs.ID, &rs.FeedID, &rs.Plugin, &rs.Config, &rs.CreatedAt,
			&rs.ScheduledStartAt, &rs.StartedAt, &rs.EndedAt,
			&rs.State, pq.Array(&rs.Errors),
			&rs.TotalDatums, &rs.TotalRetries, &rs.TotalTasks)
		if err != nil {
			return nil, err
		}
		rsArr = append(rsArr, &rs)

	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return rsArr, nil
}

// FindMissingSchedules pulls info to ask a plugin to create a schedule
func (db *DB) FindMissingSchedules(ctx context.Context, limit int) ([]*discollect.ScheduleRequest, error) {
	rows, err := db.sql.QueryContext(ctx, `
	SELECT f.id, max(f.plugin), jsonb_agg(
		row_to_json(sc.*) ORDER BY scheduled_start_at DESC
	) as scrapes, jsonb_agg(
		row_to_json(ps.*) ORDER BY ps.created_at DESC
	) FILTER (WHERE ps.id IS NOT NULL) as posts
	FROM feeds f
	JOIN LATERAL (SELECT * FROM scrapes WHERE feed_id = f.id ORDER BY scrapes.scheduled_start_at DESC LIMIT 10) sc ON true
	LEFT JOIN LATERAL (SELECT * FROM posts WHERE feed_id = f.id ORDER BY posts.posted_at DESC LIMIT 10) ps ON true
	WHERE NOT EXISTS (
		SELECT 1 FROM scrapes 
		WHERE feed_id = f.id
		AND state = 'WAITING'
	)
	GROUP BY f.id
	LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}

	var sr []*discollect.ScheduleRequest

	for rows.Next() {
		var feedID uuid.UUID
		var plugin string
		var scrapesJSON []byte
		var postsJSON []byte

		err := rows.Scan(&feedID, &plugin, &scrapesJSON, &postsJSON)
		if err != nil {
			return nil, err
		}

		var latestScrapes []*discollect.Scrape
		if len(scrapesJSON) > 0 {
			err = json.Unmarshal(scrapesJSON, &latestScrapes)
			if err != nil {
				return nil, err
			}
		}

		var latestPosts []*hydrocarbon.Post
		if len(postsJSON) > 0 {
			err = json.Unmarshal(postsJSON, &latestPosts)
			if err != nil {
				return nil, err
			}
		}

		sr = append(sr, &discollect.ScheduleRequest{
			FeedID:        feedID,
			Plugin:        plugin,
			LatestScrapes: latestScrapes,
			LatestDatums:  latestPosts,
		})
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return sr, nil
}

// InsertSchedule inserts all the schedules
func (db *DB) InsertSchedule(ctx context.Context, sr *discollect.ScheduleRequest, ss []*discollect.ScrapeSchedule) error {
	for _, s := range ss {
		_, err := db.sql.ExecContext(ctx, `
		INSERT INTO scrapes
		(feed_id, plugin, config, scheduled_start_at)
		VALUES 
		($1, $2, $3, $4)
		ON CONFLICT ON CONSTRAINT scrapes_plugin_scheduled_start_at_config_key DO NOTHING;`, sr.FeedID, sr.Plugin, s.Config, s.ScheduledStartAt)
		if err != nil {
			return err
		}
	}

	return nil
}

// EndScrape marks a scrape as SUCCESS and records the number of datums and
// tasks returned
func (db *DB) EndScrape(ctx context.Context, id uuid.UUID, datums, retries, tasks int) error {
	row := db.sql.QueryRowContext(ctx, `
	UPDATE scrapes
	SET state = 'SUCCESS'::scrape_state, ended_at = now(), total_datums = $1, total_retries = $2, total_tasks = $3
	WHERE id = $4
	RETURNING state`, datums, retries, tasks, id)

	var state string
	err := row.Scan(&state)
	if err != nil {
		return err
	}

	if state != "SUCCESS" {
		return errors.New("could not end scrape")
	}

	return nil
}

// ErrorScrape marks a scrape as ERRORED and adds the error to its list
func (db *DB) ErrorScrape(ctx context.Context, id uuid.UUID, err error) error {
	return nil
}
