package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
)

type tokenOut struct {
	Token  string `json:"access_token"`
	RToken string `json:"refresh_token"`
}

type tokenRefresher struct {
	Token  string
	RToken string
	ctx    context.Context
	m      sync.Mutex
}

type conversations struct {
	Conversations map[string]conversation `json:"conversations"`
}

type conversation struct {
	Subject string `json:"subject"`
	ID      string `json:"id"`
}

var (
	secret           string
	appID            string
	oauthURL         string
	subreddit        string
	whitelistedUsers map[string]string
	badWords         []string
)

func main() {
	// oauth URL
	secret = os.Getenv("reddit_app_secret")
	appID = os.Getenv("reddit_app_id")
	oauthURL = fmt.Sprintf("https://www.reddit.com/api/v1/authorize?client_id=%s&response_type=code&state=123456&redirect_uri=https://reddit.com&duration=permanent&scope=modcontributors,modmail,read,modposts,modflair", appID)
	subreddit = os.Getenv("reddit_app_subreddit")

	whitelistedUsers = map[string]string{
		"redditdummies123456": "",
	}

	badWords = []string{
		"upvote",
		"upvotes",
		"Upvote",
		"Upvotes",
		"UPVOTE",
		"UPVOTES",
		"downvote",
		"downvotes",
		"Downvote",
		"Downvotes",
		"DOWNVOTE",
		"DOWNVOTES",
		"updoot",
		"updoots",
		"likes",
		"karma",
	}

	ctx := context.Background()

	// trap Ctrl+C and call cancel on the context
	ctx, cancel := context.WithCancel(ctx)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	tr := tokenRefresher{}
	tr.Token, tr.RToken = getNewToken()
	tr.ctx = ctx

	file, err := os.Open("./banned_souls.txt")
	if err != nil {
		panic(err)
	}

	scanner := bufio.NewScanner(file)

	i := 0
	user := ""
	userMap := map[string]int{}
	users := map[string]string{}
	for scanner.Scan() {
		i++
		if i > 10000000 {
			break
		}
		user = scanner.Text()
		userMap[user] = i
		users[user] = user
	}
	file.Close()

	uf, err := os.Open("./finished.txt")
	if err != nil {
		panic(err)
	}
	fScanner := bufio.NewScanner(uf)
	for fScanner.Scan() {
		user = fScanner.Text()
		delete(users, user)
	}
	uf.Close()

	fmt.Println("length of users: ", len(users))
	fmt.Println("length of userMap: ", len(userMap))

	go watchModMail(&tr)
	go userInvite(users, &tr)
	go tr.refreshWatcher()
	spamQueueWatcher(&tr, &userMap)
}

func spamQueueWatcher(tr *tokenRefresher, userMap *map[string]int) {
	defer fmt.Println("spamQueueWatcher exited")
	for {
		select {
		case <-tr.ctx.Done():
			return
		default:
			f := getFilteredSpamQueue(tr, 100)
			processSpamQueue(tr, f, userMap)
			time.Sleep(1 * time.Second)
		}
	}

}

func flairUser(tr *tokenRefresher, username string, flair string) error {
	body := fmt.Sprintf("name=%s&text=%s&api_type=json", username, flair)
	tr.m.Lock()
	url := fmt.Sprintf("https://oauth.reddit.com/r/%s/api/flair", subreddit)
	resp, sOut, err := userReq(url, body, "POST", tr.Token)
	if err != nil {
		return err
	}
	tr.m.Unlock()
	if sOut != `{"json": {"errors": []}}` {
		spew.Dump(resp.StatusCode, sOut, err, "flairUser func")
		return fmt.Errorf("failed to flair %v", sOut)
	}
	return nil
}

func approvePost(tr *tokenRefresher, thing string) {
	body := fmt.Sprintf("id=%s", thing)
	tr.m.Lock()
	url := fmt.Sprintf("https://oauth.reddit.com/api/approve")
	resp, sOut, err := userReq(url, body, "POST", tr.Token)
	if err != nil {
		panic(err)
	}
	tr.m.Unlock()
	if sOut != `{}` {
		spew.Dump(resp.StatusCode, sOut, err, "approvePost func")
	}
}

func removePost(tr *tokenRefresher, thing string) {
	body := fmt.Sprintf("id=%s&spam=false", thing)
	tr.m.Lock()
	url := fmt.Sprintf("https://oauth.reddit.com/api/remove")
	resp, sOut, err := userReq(url, body, "POST", tr.Token)
	if err != nil {
		panic(err)
	}
	tr.m.Unlock()
	if sOut != `{}` {
		fmt.Println(resp.StatusCode, sOut, err, "removePost func")
	}
}

func processSpamQueue(tr *tokenRefresher, in SpamStruct, userMap *map[string]int) {
	um := *userMap
	for _, c := range in.Data.Children {
		select {
		case <-tr.ctx.Done():
			defer fmt.Println("processSpamQueue exited")
			return
		default:
			if c.Kind == "t3" {
				remove := false
				// check the title for spam triggers
				for _, s := range badWords {
					if strings.Contains(c.Data.Title, s) {
						remove = true
					}
				}
				if remove {
					fmt.Printf("User %s possible karmawhoring: %s\n", c.Data.Author, c.Data.Title)
					removePost(tr, c.Data.Name)
					time.Sleep(1 * time.Second)
					continue
				}
			}
			// only let thru if the user is snapped or whitelisted
			_, ok := whitelistedUsers[c.Data.Author]
			if ok {
				fmt.Printf("User %s is whitelisted!\n", c.Data.Author)
				approvePost(tr, c.Data.Name)
				continue
			}
			i, ok := um[c.Data.Author]
			if !ok {
				fmt.Printf("User %s not snapped!\n", c.Data.Author)
				removePost(tr, c.Data.Name)
				time.Sleep(1 * time.Second)
				continue
			}
			approvePost(tr, c.Data.Name)
			s, ok := c.Data.AuthorFlairText.(string)
			numString := strconv.Itoa(i)
			fmt.Printf("User %s snap number is %v, current flair is %v!\n", c.Data.Author, i, c.Data.AuthorFlairText)
			if ok {
				if s != numString {
					err := flairUser(tr, c.Data.Author, numString)
					if err == nil {
						fmt.Printf("User %s is now flaired %v!\n", c.Data.Author, i)
					} else {
						fmt.Printf("Could not flair user %s err %v!\n", c.Data.Author, err)
					}
				}
			} else {
				err := flairUser(tr, c.Data.Author, numString)
				if err == nil {
					fmt.Printf("User %s is now flaired %v!\n", c.Data.Author, i)
				} else {
					fmt.Printf("Could not flair user %s err %v!\n", c.Data.Author, err)
				}
			}
			time.Sleep(1 * time.Second)
		}
	}
}

func getFilteredSpamQueue(tr *tokenRefresher, count int) (final SpamStruct) {
	// read spam queue posts seen into a map
	f, err := os.Open("./spam_queue_posts_seen.txt")
	if err != nil {
		panic(err)
	}
	scanner := bufio.NewScanner(f)
	seenPosts := map[string]string{}
	for scanner.Scan() {
		txt := scanner.Text()
		seenPosts[txt] = txt
	}
	f.Close()

	out := getSpamQueue(tr, "")
	lOut := len(out.Data.Children)
	tempOut := SpamStruct{}
	i := 0
	done := false
	//fmt.Println("pulling spam queue")
	for {
		if done {
			break
		}
		select {
		case <-tr.ctx.Done():
			defer fmt.Println("spamQueue exited")
			return
		default:
			i++
			if i > count {
				done = true
				break
			}
			if i > 1 {
				if lOut > 0 {
					lastPost := out.Data.Children[lOut-1].Data.Name
					out = getSpamQueue(tr, lastPost)
				}
			}
			time.Sleep(1 * time.Second)
			if lOut > 0 {
				lOut = len(out.Data.Children)
				for _, c := range out.Data.Children {
					_, ok := seenPosts[c.Data.Name]
					if ok {
						// set done to true if we've seen this post in the spam queue before
						done = true
					}
					tempOut.Data.Children = append(tempOut.Data.Children, c)
				}
			} else {
				done = true
				break
			}
		}
	}

	outStrings := []string{}
	// filter to only automod spam
	for _, c := range tempOut.Data.Children {
		if c.Data.BannedBy.string == `"AutoModerator"` {
			final.Data.Children = append(final.Data.Children, c)
		}
		outStrings = append(outStrings, c.Data.Name)
	}
	for p := range seenPosts {
		outStrings = append(outStrings, p)
	}
	newSpamList := strings.Join(outStrings, "\n")
	err = ioutil.WriteFile("./spam_queue_posts_seen.txt", []byte(newSpamList), 0644)
	if err != nil {
		panic(err)
	}
	if len(final.Data.Children) > 0 {
		fmt.Printf("spam queue filtering finished, preCount: %v count: %v\n", len(tempOut.Data.Children), len(final.Data.Children))
	}
	return final
}

func getSpamQueue(tr *tokenRefresher, after string) SpamStruct {
	body := fmt.Sprintf("")
	url := fmt.Sprintf("https://oauth.reddit.com/r/%s/about/spam?limit=25", subreddit)
	if after != "" {
		url = url + fmt.Sprintf("&after=%s", after)
	}
	// deal with request timeout
	incomplete := true
	sr := SpamStruct{}
	for incomplete {
		tr.m.Lock()
		resp, sOut, err := userReq(url, body, "GET", tr.Token)
		if err != nil {
			fmt.Printf("err in userReq in getSpamQueue: %v\n", err)
			continue
		}
		tr.m.Unlock()

		err = json.Unmarshal([]byte(sOut), &sr)
		if err != nil {
			fmt.Println(resp.StatusCode)
			fmt.Println(sOut)
			fmt.Printf("err in getSpamQueue: %v\n", err)
			continue
		}
		incomplete = false
	}
	return sr
}

func watchModMail(tr *tokenRefresher) {
	defer fmt.Println("watchModMail exited")
	for {
		select {
		case <-tr.ctx.Done():
			return
		default:
			convos := getModMail(tr)
			for _, c := range convos.Conversations {
				select {
				case <-tr.ctx.Done():
					return
				default:
					if c.Subject == "you are an approved submitter" {
						fmt.Println("archiving ", c.ID)
						archiveMail(tr, c.ID)
						time.Sleep(3 * time.Second)
					}
				}
			}
			time.Sleep(3 * time.Second)
		}
	}
}

func archiveMail(tr *tokenRefresher, mailID string) {
	body := fmt.Sprintf("conversation_id=%s", mailID)
	tr.m.Lock()
	resp, _, err := userReq(fmt.Sprintf("https://oauth.reddit.com/api/mod/conversations/%s/archive", mailID), body, "POST", tr.Token)
	if err != nil {
		panic(err)
	}
	tr.m.Unlock()

	fmt.Println("archived mail: ", resp.StatusCode, err)
}

func getModMail(tr *tokenRefresher) conversations {
	body := fmt.Sprintf("")
	tr.m.Lock()
	url := fmt.Sprintf("https://oauth.reddit.com/api/mod/conversations?sort=recent&limit=500&entity=%s&state=notifications", subreddit)
	resp, sOut, err := userReq(url, body, "GET", tr.Token)
	if err != nil {
		panic(err)
	}
	tr.m.Unlock()

	convos := conversations{}
	err = json.Unmarshal([]byte(sOut), &convos)
	if err != nil {
		fmt.Println(resp.StatusCode)
		fmt.Println(sOut)
		panic(err)
	}
	if len(convos.Conversations) > 0 {
		fmt.Printf("%v convos retrieved\n", len(convos.Conversations))
	}
	return convos
}

func (tr *tokenRefresher) refreshWatcher() {
	i := 0
	for {
		select {
		case <-tr.ctx.Done():
			defer fmt.Println("refreshWatcher exited")
			return
		default:
			i++
			if i > 60 {
				i = 0
				//fmt.Println("refreshing token")
				tr.m.Lock()
				tr.Token, _ = refreshToken(tr.RToken)
				tr.m.Unlock()
			}
			time.Sleep(5 * time.Second)
		}
	}
}

func userInvite(usersIn map[string]string, tr *tokenRefresher) {
	i := 0
	f, err := os.OpenFile("./finished.txt", os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	for _, u := range usersIn {
		i++
		cont := false

		for !cont {
			select {
			case <-tr.ctx.Done():
				defer fmt.Println("userInvite exited")
				return
			default:
				body := fmt.Sprintf("api_type=json&type=contributor&name=%s", u)
				tr.m.Lock()
				url := fmt.Sprintf("https://oauth.reddit.com/r/%s/api/friend", subreddit)
				resp, sOut, err := userReq(url, body, "POST", tr.Token)
				if err != nil {
					panic(err)
				}
				tr.m.Unlock()

				fmt.Println(u, resp.StatusCode, sOut, i)
				time.Sleep(1500 * time.Millisecond)
				if sOut != `{"json": {"errors": []}}` {
					if sOut == `{"json": {"errors": [["SUBREDDIT_RATELIMIT", "you are doing that too much. try again later.", null]]}}` {
						fmt.Println("rate limit reached, sleeping 30 minutes")
						for i := 0; i < 1800; i++ {
							select {
							case <-tr.ctx.Done():
								defer fmt.Println("userInvite exited")
								return
							default:
								time.Sleep(1 * time.Second)
							}
						}
					}
					if sOut == `{"json": {"errors": [["USER_DOESNT_EXIST", "that user doesn't exist", "name"]]}}` {
						cont = true
						_, err = f.WriteString(u + "\n")
						if err != nil {
							panic(err)
						}
						err = f.Sync()
						if err != nil {
							panic(err)
						}
						delete(usersIn, u)
						fmt.Printf("user %s no longer exists, skipping\n", u)
					}
				} else {
					cont = true
					_, err = f.WriteString(u + "\n")
					if err != nil {
						panic(err)
					}
					err = f.Sync()
					if err != nil {
						panic(err)
					}
					delete(usersIn, u)
					fmt.Printf("user: %s added as an approved submitter\n", u)
				}
			}
		}
	}
}

func botReq(url, bodyIn, typ string) (resp *http.Response, bodyOut string, err error) {
	bReader := strings.NewReader(bodyIn)
	req, err := http.NewRequest(typ, url, bReader)
	if err != nil {
		panic(err)
	}
	req.Header.Set("User-Agent", "linux:moderator:v1.0.0 (by /u/SomebodyOnceToldMe")
	req.SetBasicAuth(appID, secret)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	bOut, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	return resp, string(bOut), nil
}

func userReq(url, bodyIn, typ, token string) (resp *http.Response, bodyOut string, err error) {
	requestIncomplete := true
	i := 0
	for requestIncomplete {
		i++
		if i > 15 {
			panic(fmt.Sprintf("too many retries in userReq, url: %s", url))
		}
		bReader := strings.NewReader(bodyIn)
		req, err := http.NewRequest(typ, url, bReader)
		if err != nil {
			panic(err)
		}
		req.Header.Set("User-Agent", "moderatorbot AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			fmt.Printf("http error during userReq: %v \n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		bOut, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == 429 {
				fmt.Println("request got 429'd, sleeping for a second before retrying.")
				time.Sleep(1 * time.Second)
				continue
			}
			fmt.Printf("status code not ok: status: %v out %s\n", resp.StatusCode, string(bOut))
			time.Sleep(1 * time.Second)
			continue
		}
		requestIncomplete = false
		return resp, string(bOut), nil
	}
	return resp, bodyOut, fmt.Errorf("unreachable userReq code")
}

func getNewToken() (string, string) {
	if _, err := os.Stat("./refreshtoken.txt"); err == nil {
		rToken := ""
		f, err := os.Open("./refreshtoken.txt")
		if err != nil {
			panic(err)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			rToken = scanner.Text()
			break
		}
		f.Close()

		token, _ := refreshToken(rToken)
		return token, rToken
	}

	//// GET new token
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("Go to here and get auth code: %s\n", oauthURL)
	fmt.Print("Enter code: ")
	authCode, _ := reader.ReadString('\n')
	authCode = strings.Trim(authCode, "\n")
	bIn := fmt.Sprintf("grant_type=authorization_code&code=%s&redirect_uri=https://reddit.com", authCode)
	_, sOut, err := botReq("https://www.reddit.com/api/v1/access_token", bIn, "POST")
	if err != nil {
		panic(err)
	}
	// get creds
	tOut := tokenOut{}
	err = json.Unmarshal([]byte(sOut), &tOut)
	if err != nil {
		panic(err)
	}

	if tOut.RToken == "" {
		panic("refreshToken is blank")
	}

	if tOut.Token == "" {
		panic("token is blank")
	}

	// cache the refresh token for restarts
	err = ioutil.WriteFile("./refreshtoken.txt", []byte(tOut.RToken), 0644)
	if err != nil {
		panic(err)
	}

	//fmt.Println(fmt.Sprintf("token: %s refreshToken: %s", tOut.Token, tOut.RToken))
	return tOut.Token, tOut.RToken
}

func refreshToken(refreshToken string) (string, string) {
	//// Refresh the token
	incomplete := true
	tOut := tokenOut{}
	for incomplete {
		bIn := fmt.Sprintf("grant_type=refresh_token&refresh_token=%s&duration=permanent", refreshToken)
		_, sOut, err := botReq("https://www.reddit.com/api/v1/access_token", bIn, "POST")
		if err != nil {
			fmt.Printf("http error during refreshToken: %v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		// get creds
		err = json.Unmarshal([]byte(sOut), &tOut)
		if err != nil {
			fmt.Printf("json error during refreshToken: %v\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		fmt.Println(fmt.Sprintf("token: %s refreshToken: %s", tOut.Token, tOut.RToken))
		incomplete = false
	}
	return tOut.Token, tOut.RToken
}

type boolString struct {
	string
}

// UnmarshalJSON is our custom unmarshaller
func (foo *boolString) UnmarshalJSON(d []byte) error {
	if d[0] != '"' {
		d = []byte(`"` + string(d) + `"`)
	}

	foo.string = string(d)
	return nil
}

type t3Struct struct {
	Data struct {
	} `json:"data"`
}

// SpamStruct is a struct representing a spam listing payload from reddit
type SpamStruct struct {
	Data struct {
		After    string      `json:"after"`
		Before   interface{} `json:"before"`
		Children []struct {
			Data struct {
				Approved                   bool          `json:"approved"`
				ApprovedAtUtc              interface{}   `json:"approved_at_utc"`
				ApprovedBy                 interface{}   `json:"approved_by"`
				Archived                   bool          `json:"archived"`
				Author                     string        `json:"author"`
				AuthorFlairBackgroundColor interface{}   `json:"author_flair_background_color"`
				AuthorFlairCSSClass        interface{}   `json:"author_flair_css_class"`
				AuthorFlairRichtext        []interface{} `json:"author_flair_richtext"`
				AuthorFlairTemplateID      interface{}   `json:"author_flair_template_id"`
				AuthorFlairText            interface{}   `json:"author_flair_text"`
				AuthorFlairTextColor       interface{}   `json:"author_flair_text_color"`
				AuthorFlairType            string        `json:"author_flair_type"`
				BanNote                    string        `json:"ban_note"`
				BannedAtUtc                float64       `json:"banned_at_utc"`
				BannedBy                   boolString    `json:"banned_by"`
				Body                       string        `json:"body"`
				BodyHTML                   string        `json:"body_html"`
				CanGild                    bool          `json:"can_gild"`
				CanModPost                 bool          `json:"can_mod_post"`
				Collapsed                  bool          `json:"collapsed"`
				CollapsedReason            interface{}   `json:"collapsed_reason"`
				Controversiality           int           `json:"controversiality"`
				Created                    float64       `json:"created"`
				CreatedUtc                 float64       `json:"created_utc"`
				Distinguished              interface{}   `json:"distinguished"`
				Downs                      int           `json:"downs"`
				Edited                     boolString    `json:"edited"`
				Gilded                     int           `json:"gilded"`
				ID                         string        `json:"id"`
				IgnoreReports              bool          `json:"ignore_reports"`
				IsSubmitter                bool          `json:"is_submitter"`
				Likes                      interface{}   `json:"likes"`
				LinkAuthor                 string        `json:"link_author"`
				LinkID                     string        `json:"link_id"`
				LinkPermalink              string        `json:"link_permalink"`
				LinkTitle                  string        `json:"link_title"`
				LinkURL                    string        `json:"link_url"`
				ModNote                    interface{}   `json:"mod_note"`
				ModReasonBy                interface{}   `json:"mod_reason_by"`
				ModReasonTitle             interface{}   `json:"mod_reason_title"`
				ModReports                 []interface{} `json:"mod_reports"`
				Name                       string        `json:"name"`
				NoFollow                   bool          `json:"no_follow"`
				NumComments                int           `json:"num_comments"`
				NumReports                 int           `json:"num_reports"`
				Over18                     bool          `json:"over_18"`
				ParentID                   string        `json:"parent_id"`
				Permalink                  string        `json:"permalink"`
				Quarantine                 bool          `json:"quarantine"`
				RemovalReason              interface{}   `json:"removal_reason"`
				Removed                    bool          `json:"removed"`
				Replies                    string        `json:"replies"`
				ReportReasons              []interface{} `json:"report_reasons"`
				RteMode                    string        `json:"rte_mode"`
				Saved                      bool          `json:"saved"`
				Score                      int           `json:"score"`
				ScoreHidden                bool          `json:"score_hidden"`
				SendReplies                bool          `json:"send_replies"`
				Spam                       bool          `json:"spam"`
				Stickied                   bool          `json:"stickied"`
				Subreddit                  string        `json:"subreddit"`
				SubredditID                string        `json:"subreddit_id"`
				SubredditNamePrefixed      string        `json:"subreddit_name_prefixed"`
				SubredditType              string        `json:"subreddit_type"`
				Ups                        int           `json:"ups"`
				UserReports                []interface{} `json:"user_reports"`
				SelfText                   string        `json:"selftext"`
				Title                      string        `json:"title"`
			} `json:"data"`
			Kind string `json:"kind"`
		} `json:"children"`
		Dist    int         `json:"dist"`
		Modhash interface{} `json:"modhash"`
	} `json:"data"`
	Kind string `json:"kind"`
}
