package redditbot

import (
	"errors"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/json"
	"github.com/disgoorg/log"
	"github.com/disgoorg/snowflake/v2"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// RequestsPerMinute is the number of requests per minute we can make to the reddit API. This is 600 requests per 10 minutes.
	RequestsPerMinute = 590 / 10

	// WaitTime is the time we wait between requests to the reddit API to avoid hitting the ratelimit.
	WaitTime = time.Minute / RequestsPerMinute
)

var (
	ErrSubredditNotFound  = errors.New("subreddit not found")
	ErrSubredditForbidden = errors.New("subreddit forbidden")
)

func (b *Bot) AddSubscription(sub Subscription) error {
	if err := b.DB.AddSubscription(sub); err != nil {
		return err
	}

	subreddits.With(prometheus.Labels{
		"subreddit":  sub.Subreddit,
		"webhook_id": strconv.FormatInt(int64(sub.WebhookID), 10),
		"guild_id":   strconv.FormatInt(int64(sub.GuildID), 10),
		"channel_id": strconv.FormatInt(int64(sub.ChannelID), 10),
	}).Inc()

	return nil
}

func (b *Bot) RemoveSubscription(webhookID snowflake.ID) error {
	sub, err := b.DB.RemoveSubscription(webhookID)
	if err != nil {
		return err
	}

	subreddits.With(prometheus.Labels{
		"subreddit":  sub.Subreddit,
		"webhook_id": strconv.FormatInt(int64(sub.WebhookID), 10),
		"guild_id":   strconv.FormatInt(int64(sub.GuildID), 10),
		"channel_id": strconv.FormatInt(int64(sub.ChannelID), 10),
	}).Dec()

	return nil
}

func (b *Bot) RemoveSubscriptionByGuildSubreddit(guildID snowflake.ID, subreddit string, reason string) error {
	sub, err := b.DB.RemoveSubscriptionByGuildSubreddit(guildID, subreddit)
	if err != nil {
		return err
	}

	_ = b.Client.Rest().DeleteWebhookWithToken(sub.WebhookID, sub.WebhookToken, rest.WithReason(reason))

	subreddits.With(prometheus.Labels{
		"subreddit":  sub.Subreddit,
		"webhook_id": strconv.FormatInt(int64(sub.WebhookID), 10),
		"guild_id":   strconv.FormatInt(int64(sub.GuildID), 10),
		"channel_id": strconv.FormatInt(int64(sub.ChannelID), 10),
	}).Dec()

	return nil
}

func (b *Bot) ListenSubreddits() {
	for {
		now := time.Now()
		subscriptions, err := b.DB.GetAllSubscriptions()
		if err != nil {
			log.Error("error getting subscriptions:", err.Error())
			continue
		}
		log.Debugf("checking subreddits for %d subscriptions", len(subscriptions))

		for i := range subscriptions {
			subNow := time.Now()
			ok, err := b.DB.HasSubscription(subscriptions[i].WebhookID)
			if err != nil {
				log.Errorf("error checking subscription for webhook %s: %s", subscriptions[i].WebhookID, err.Error())
				continue
			}
			if !ok {
				continue
			}

			b.checkSubreddit(subscriptions[i])

			waitTime := WaitTime - time.Now().Sub(subNow)
			if waitTime > 0 {
				<-time.After(WaitTime)
			}
		}

		duration := time.Now().Sub(now)
		if duration > time.Duration(len(subscriptions))*WaitTime {
			log.Debugf("took %s too long to check %d subreddits", duration.String(), len(subscriptions))
		}
	}
}

func (b *Bot) checkSubreddit(subscription Subscription) {
	lastPost := b.LastPosts[subscription.WebhookID]
	posts, before, err := b.Reddit.GetPosts(b.Reddit, subscription.Subreddit, lastPost)
	if err != nil {
		log.Errorf("error getting posts for subreddit %s: %s", subscription.Subreddit, err.Error())
		if errors.Is(err, ErrSubredditNotFound) || errors.Is(err, ErrSubredditForbidden) {
			if err = b.RemoveSubscription(subscription.WebhookID); err != nil {
				log.Errorf("error removing subscription for webhook %s: %s", subscription.WebhookID, err.Error())
			}
		}
		return
	}
	log.Debugf("got %d posts for subreddit %s before: %v\n", len(posts), subscription.Subreddit, before)

	if lastPost != "" {
		for i := len(posts) - 1; i >= 0; i-- {
			b.sendPost(subscription, posts[i])
		}
	}
	if before != "" {
		b.LastPosts[subscription.WebhookID] = before
	}
}

func (b *Bot) sendPost(subscription Subscription, post RedditPost) {
	embed := discord.Embed{
		Title:       post.Title,
		Description: html.UnescapeString(post.Selftext),
		URL:         "https://reddit.com" + post.Permalink,
		Author: &discord.EmbedAuthor{
			Name:    "new post in " + post.SubredditNamePrefixed,
			URL:     "https://reddit.com/" + post.SubredditNamePrefixed,
			IconURL: "https://www.redditstatic.com/desktop2x/img/favicon/android-icon-192x192.png",
		},
		Footer: &discord.EmbedFooter{
			Text: "posted by " + post.Author,
		},
		Timestamp: json.Ptr(time.Unix(int64(post.CreatedUtc), 0)),
	}

	postsSent.With(prometheus.Labels{
		"subreddit":  subscription.Subreddit,
		"webhook.id": strconv.FormatUint(uint64(subscription.WebhookID), 10),
		"guild.id":   strconv.FormatUint(uint64(subscription.GuildID), 10),
		"channel.id": strconv.FormatUint(uint64(subscription.ChannelID), 10),
	}).Inc()

	if b.Cfg.TestMode {
		log.Debugf("sending post to webhook %d: %s", subscription.WebhookID, post.Title)
		return
	}

	if _, err := b.Client.Rest().CreateWebhookMessage(subscription.WebhookID, subscription.WebhookToken, discord.WebhookMessageCreate{
		AvatarURL: "https://www.redditstatic.com/desktop2x/img/favicon/android-icon-192x192.png",
		Embeds:    []discord.Embed{embed},
	}, false, 0); err != nil {
		var restError rest.Error
		if errors.Is(err, &restError) && restError.Response.StatusCode == http.StatusNotFound {
			if err = b.RemoveSubscription(subscription.WebhookID); err != nil {
				log.Errorf("error removing subscription for webhook %s: %s", subscription.WebhookID, err.Error())
			}
			return
		}
		log.Errorf("error sending post to webhook %d: %s", subscription.WebhookID, err.Error())
	}
}

type RedditResponse struct {
	Data struct {
		Before   string `json:"before"`
		Children []struct {
			Data RedditPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type RedditPost struct {
	Selftext              string  `json:"selftext"`
	AuthorFullname        string  `json:"author_fullname"`
	Title                 string  `json:"title"`
	SubredditNamePrefixed string  `json:"subreddit_name_prefixed"`
	ID                    string  `json:"id"`
	Name                  string  `json:"name"`
	Author                string  `json:"author"`
	Url                   string  `json:"url"`
	Permalink             string  `json:"permalink"`
	CreatedUtc            float64 `json:"created_utc"`
}
