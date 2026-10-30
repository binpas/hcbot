package bot

import (
	"cmp"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/binpas/hcbot/internal/db"
)

// Tours (mod only): a forum thread for each show of a band's tour.
//
//	/tour upload   read a CSV or JSON file of shows into the tours table
//	/tour setup    choose the forum, and edit the title and post templates
//	/tour post     post the new shows, and update the changed ones
//	/tour edit     change the tour name or the shows
//	/tour help     the file format and the tokens, with a sample file
//
// Members can reply in the threads; only mods and the bot can make new
// ones. The bot checks the forum's permissions, but does not change them
// (except on a forum that it makes). A tour whose last show has passed
// does not show in the menus. The bot never deletes threads or tours.
//
// Custom IDs (the tour ID is in each):
//
//	tour:pick:<setup|post|edit>   select menu: the tour to work on
//	tour:forum:<id>               channel menu: the forum
//	tour:tt:<id>, tour:pt:<id>    edit the title or the post template (tourmodal:tt|pt:<id>)
//	tour:newforum:<id>            make a new forum (tourmodal:newforum:<id>)
//	tour:name:<id>                rename the tour (tourmodal:name:<id>)
//	tour:page:<id>:<page>         edit 5 shows (tourmodal:shows:<id>)

const (
	tourShowsPerPage = 5
	tourPageButtons  = 20
	maxTourTemplate  = 2000
)

func (b *Bot) registerTours() {
	file := optAttachment("file", "A CSV or JSON file of shows. See /tour help.", true)
	b.addGroup(&group{name: "tour", description: "Make a forum thread for each show of a tour (mod only).", defaultPerm: manageMessages})
	b.addCommand(&command{name: "tour upload", description: "Read a CSV or JSON file of shows. New dates are added; changed dates are updated.",
		options: []*discordgo.ApplicationCommandOption{file}, perm: permMod, handler: b.cmdTourUpload})
	b.addCommand(&command{name: "tour setup", description: "Choose the forum of a tour, and edit its thread title and post templates.",
		perm: permMod, handler: func(c *Ctx) { b.tourPicker(c, "setup") }})
	b.addCommand(&command{name: "tour post", description: "Preview, then post the new shows of a tour and update the changed ones.",
		perm: permMod, handler: func(c *Ctx) { b.tourPicker(c, "post") }})
	b.addCommand(&command{name: "tour edit", description: "Change the name or the shows of a tour.",
		perm: permMod, handler: func(c *Ctx) { b.tourPicker(c, "edit") }})
	b.addCommand(&command{name: "tour help", description: "How to write the file and the templates, with a sample file.",
		perm: permMod, handler: b.cmdTourHelp})
	b.addComponent("tour", b.onTourComponent)
	b.addModal("tourmodal", b.onTourModal)
	b.addConfirm("tourupload", b.onTourUploadConfirm)
	b.addConfirm("tourpost", b.onTourPostConfirm)
}

// tourToday is today's date in Pacific time, YYYY-MM-DD.
func tourToday() string { return time.Now().In(reminderZone).Format(time.DateOnly) }

func tourID(c *Ctx, i int) int64 {
	if len(c.Args) <= i {
		return 0
	}
	id, _ := strconv.ParseInt(c.Args[i], 10, 64)
	return id
}

// loadTour loads a tour of this server. When it returns nil, it has
// already answered.
func (b *Bot) loadTour(c *Ctx, id int64) *db.Tour {
	t, err := b.db.GetTour(c.GuildID(), id)
	if err != nil {
		c.dbFailed("load tour", err)
		return nil
	}
	if t == nil {
		c.Reply(true, errEmbed("That tour does not exist anymore."))
	}
	return t
}

// tourPicker answers /tour setup, post, and edit: a menu of the tours with
// upcoming shows, or the tour's view when there is only one.
func (b *Bot) tourPicker(c *Ctx, action string) {
	tours, err := b.db.ActiveTours(c.GuildID(), tourToday())
	if err != nil {
		c.dbFailed("list tours", err)
		return
	}
	switch len(tours) {
	case 0:
		c.Reply(true, errEmbed("There are no tours with upcoming shows. Use `/tour upload` to add one, and `/tour help` for the file format."))
		return
	case 1:
		embeds, components := b.tourView(c, action, &tours[0])
		c.ReplyComplex(true, embeds, components)
		return
	}
	var options []discordgo.SelectMenuOption
	for _, t := range tours[:min(len(tours), 25)] {
		options = append(options, discordgo.SelectMenuOption{Label: truncateEllipsis(t.Name, 100), Value: strconv.FormatInt(t.ID, 10)})
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("🎸 Choose a tour", "", colourBlurple)},
		[]discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.StringSelectMenu, CustomID: "tour:pick:" + action, Placeholder: "Tour", Options: options}}}})
}

func (b *Bot) tourView(c *Ctx, action string, t *db.Tour) ([]*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	var embed *discordgo.MessageEmbed
	var components []discordgo.MessageComponent
	var err error
	switch action {
	case "setup":
		embed, components, err = b.tourSetupView(t)
	case "post":
		embed, components, err = b.tourPostView(c, t)
	default:
		embed, components, err = b.tourEditView(t)
	}
	if err != nil {
		c.logErr("tour "+action, err)
		return []*discordgo.MessageEmbed{errEmbed("Something went wrong with the database — check the bot's log.")}, []discordgo.MessageComponent{}
	}
	return []*discordgo.MessageEmbed{embed}, components
}

// --- /tour upload ---

// tourUpload is the data behind the Confirm button of an upload.
type tourUpload struct {
	Rows []tourRow
}

func (b *Bot) cmdTourUpload(c *Ctx) {
	data, ok := b.readAttachment(c, c.Attachment("file"), maxTourFile, fmt.Sprintf("The file is too large. The limit is %d KB.", maxTourFile>>10))
	if !ok {
		return
	}
	rows, problems := parseTourFile(data)
	if len(problems) > 0 {
		c.Reply(true, errEmbed("I could not use the file:\n- "+strings.Join(problems, "\n- ")+"\n\nUse `/tour help` for the file format."))
		return
	}
	summary, err := b.tourUploadSummary(c.GuildID(), rows)
	if err != nil {
		var limit tourLimitError
		if errors.As(err, &limit) {
			c.Reply(true, errEmbed(limit.Error()))
			return
		}
		c.dbFailed("tour upload", err)
		return
	}
	id, err := b.savePending("tourupload", c.UserID(), tourUpload{Rows: rows})
	if err != nil {
		c.dbFailed("save pending tour upload", err)
		return
	}
	c.ReplyComplex(true, []*discordgo.MessageEmbed{makeEmbed("📥 Upload tour shows", summary, colourOrange)},
		confirmButtons("tourupload", c.UserID(), id))
}

type tourLimitError string

func (e tourLimitError) Error() string { return string(e) }

// tourGroups groups rows by tour name (any case), in file order.
func tourGroups(rows []tourRow) [][]tourRow {
	var out [][]tourRow
	index := map[string]int{}
	for _, r := range rows {
		k := strings.ToLower(r.Tour)
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, nil)
		}
		out[i] = append(out[i], r)
	}
	return out
}

// tourUploadSummary tells what an upload will do for each tour.
func (b *Bot) tourUploadSummary(guildID string, rows []tourRow) (string, error) {
	var sb strings.Builder
	for _, g := range tourGroups(rows) {
		name := g[0].Tour
		existing := map[string]map[string]string{}
		t, err := b.db.TourByName(guildID, name)
		if err != nil {
			return "", err
		}
		if t != nil {
			name = t.Name
			shows, err := b.db.TourShows(t.ID)
			if err != nil {
				return "", err
			}
			for _, s := range shows {
				existing[s.Date] = s.Fields
			}
		}
		var added, changed, same int
		for _, r := range g {
			switch old, ok := existing[r.Date]; {
			case !ok:
				added++
			case maps.Equal(old, r.Fields):
				same++
			default:
				changed++
			}
		}
		if len(existing)+added > maxTourShows {
			return "", tourLimitError(fmt.Sprintf("**%s** would have %d shows. The limit is %d.", name, len(existing)+added, maxTourShows))
		}
		state := "a new tour"
		if t != nil {
			state = "a tour that exists"
		}
		fmt.Fprintf(&sb, "**%s** (%s): %d new, %d changed, %d with no change.\n", name, state, added, changed, same)
	}
	sb.WriteString("\nShows that are not in the file do not change. Then use `/tour setup` and `/tour post`.")
	return sb.String(), nil
}

func (b *Bot) onTourUploadConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	var up tourUpload
	if !b.takePending(c, "tourupload", extra[0], &up) {
		return
	}
	if _, err := b.tourUploadSummary(c.GuildID(), up.Rows); err != nil { // the limits again
		c.Update([]*discordgo.MessageEmbed{errEmbed(err.Error())}, []discordgo.MessageComponent{})
		return
	}
	var sb strings.Builder
	for _, g := range tourGroups(up.Rows) {
		t, err := b.db.TourByName(c.GuildID(), g[0].Tour)
		if err == nil && t == nil {
			t = &db.Tour{GuildID: c.GuildID(), Name: g[0].Tour, TitleTemplate: defaultTourTitle, PostTemplate: defaultTourPost}
			t.ID, err = b.db.CreateTour(*t)
		}
		if err != nil {
			c.logErr("tour upload", err)
			fmt.Fprintf(&sb, "**%s**: the database failed. Check the bot's log.\n", g[0].Tour)
			continue
		}
		var added, changed, failed int
		for _, r := range g {
			a, ch, err := b.db.UpsertTourShow(t.ID, r.Date, r.Fields)
			switch {
			case err != nil:
				c.logErr("tour upload show", err)
				failed++
			case a:
				added++
			case ch:
				changed++
			}
		}
		fmt.Fprintf(&sb, "**%s**: %d new, %d changed.", t.Name, added, changed)
		if failed > 0 {
			fmt.Fprintf(&sb, " %d failed (check the bot's log).", failed)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\nNext: `/tour setup` to choose the forum and the templates, then `/tour post`.")
	c.Update([]*discordgo.MessageEmbed{makeEmbed("✅ Shows saved", sb.String(), colourGreen)}, []discordgo.MessageComponent{})
}

// --- /tour setup ---

func (b *Bot) tourSetupView(t *db.Tour) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	shows, err := b.db.TourShows(t.ID)
	if err != nil {
		return nil, nil, err
	}
	e := makeEmbed("🎸 "+t.Name, "", colourBlurple)
	forum := "Not set. Choose a forum below, or make a new one."
	if t.ForumID != "" {
		forum = channelMention(t.ForumID)
	}
	posted := 0
	for _, s := range shows {
		if s.ThreadID != "" {
			posted++
		}
	}
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "Forum", Value: forum},
		{Name: "Shows", Value: fmt.Sprintf("%d shows, %d posted", len(shows), posted)},
		{Name: "Title template", Value: codeBlock(t.TitleTemplate)},
		{Name: "Post template", Value: codeBlock(t.PostTemplate)},
	}
	if warn := b.tourForumProblems(t); len(warn) > 0 {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "⚠️ Forum", Value: "- " + strings.Join(warn, "\n- ")})
	}
	if s := nextTourShow(shows); s != nil {
		text := renderTourShow(t, *s, tourColumns(shows))
		e.Fields = append(e.Fields, tourPreviewFields(text)...)
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "When the settings are correct, use /tour post. /tour help lists the tokens."}
	id := strconv.FormatInt(t.ID, 10)
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			MenuType: discordgo.ChannelSelectMenu, CustomID: "tour:forum:" + id, Placeholder: "Choose the forum",
			ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildForum}, MaxValues: 1}}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Edit title template", Style: discordgo.PrimaryButton, CustomID: "tour:tt:" + id},
			discordgo.Button{Label: "Edit post template", Style: discordgo.PrimaryButton, CustomID: "tour:pt:" + id},
			discordgo.Button{Label: "New forum…", Style: discordgo.SecondaryButton, CustomID: "tour:newforum:" + id},
		}},
	}
	return e, components, nil
}

func codeBlock(s string) string {
	return "```\n" + truncateEllipsis(strings.ReplaceAll(s, "```", "'''"), 1000) + "\n```"
}

// nextTourShow is the first show on or after today, else the last show.
func nextTourShow(shows []db.TourShow) *db.TourShow {
	today := tourToday()
	for i := range shows {
		if shows[i].Date >= today {
			return &shows[i]
		}
	}
	if len(shows) > 0 {
		return &shows[len(shows)-1]
	}
	return nil
}

func tourPreviewFields(text tourText) []*discordgo.MessageEmbedField {
	if len(text.Problems) > 0 {
		return []*discordgo.MessageEmbedField{{Name: "❌ Problems", Value: truncateEllipsis("- "+strings.Join(text.Problems, "\n- "), 1024)}}
	}
	return []*discordgo.MessageEmbedField{
		{Name: "Preview: thread title", Value: text.Title},
		{Name: "Preview: post", Value: truncateEllipsis(text.Post, 1024)},
	}
}

// tourForumProblems checks that members can reply in the forum's threads,
// and cannot make threads. It does not change the permissions.
func (b *Bot) tourForumProblems(t *db.Tour) []string {
	if t.ForumID == "" {
		return nil
	}
	ch, err := b.s.State.Channel(t.ForumID)
	if err != nil {
		if ch, err = b.s.Channel(t.ForumID); err != nil {
			return []string{"I cannot find the forum. Choose another one."}
		}
	}
	if ch.Type != discordgo.ChannelTypeGuildForum {
		return []string{channelMention(ch.ID) + " is not a forum."}
	}
	g, err := b.s.State.Guild(t.GuildID)
	if err != nil {
		return nil
	}
	everyone := channelPerms(g, ch, "", nil)
	var out []string
	if everyone&discordgo.PermissionViewChannel == 0 {
		out = append(out, "Members cannot see the forum. Allow **View Channel** for @everyone.")
	} else {
		if everyone&discordgo.PermissionSendMessages != 0 {
			out = append(out, "Members can make new threads. Deny **Create Posts** for @everyone.")
		}
		if everyone&discordgo.PermissionSendMessagesInThreads == 0 {
			out = append(out, "Members cannot reply in the threads. Allow **Send Messages in Posts** for @everyone.")
		}
	}
	if mod := b.cfg.ID("MOD_ROLE"); mod != "" && channelPerms(g, ch, "", []string{mod})&discordgo.PermissionSendMessages == 0 {
		out = append(out, "Mods cannot make new threads. Allow **Create Posts** for "+roleMention(mod)+".")
	}
	return out
}

// tourForumOverwrites are the permissions of a forum that the bot makes.
func (b *Bot) tourForumOverwrites(guildID string) []*discordgo.PermissionOverwrite {
	out := []*discordgo.PermissionOverwrite{{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole,
		Allow: discordgo.PermissionSendMessagesInThreads,
		Deny:  discordgo.PermissionSendMessages | discordgo.PermissionCreatePublicThreads | discordgo.PermissionCreatePrivateThreads}}
	if mod := b.cfg.ID("MOD_ROLE"); mod != "" {
		out = append(out, &discordgo.PermissionOverwrite{ID: mod, Type: discordgo.PermissionOverwriteTypeRole,
			Allow: discordgo.PermissionSendMessages | discordgo.PermissionSendMessagesInThreads})
	}
	return out
}

// --- /tour post ---

// tourPlan is what /tour post will do.
type tourPlan struct {
	add, change []db.TourShow // add: the next show first
	text        map[int64]tourText
	problems    []string
}

func (b *Bot) planTourPost(t *db.Tour) (*tourPlan, error) {
	shows, err := b.db.TourShows(t.ID)
	if err != nil {
		return nil, err
	}
	p := &tourPlan{text: map[int64]tourText{}}
	cols, today := tourColumns(shows), tourToday()
	for _, s := range shows {
		if s.ThreadID == "" && s.Date < today {
			continue // the show has passed
		}
		text := renderTourShow(t, s, cols)
		p.text[s.ID] = text
		for _, pr := range text.Problems {
			if !slices.Contains(p.problems, pr) {
				p.problems = append(p.problems, pr)
			}
		}
		switch {
		case s.ThreadID == "":
			p.add = append(p.add, s)
		case text.Title != s.PostedTitle || text.Post != s.PostedPost:
			p.change = append(p.change, s)
		}
	}
	return p, nil
}

func (b *Bot) tourPostView(c *Ctx, t *db.Tour) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	p, err := b.planTourPost(t)
	if err != nil {
		return nil, nil, err
	}
	e := makeEmbed("🎸 Post "+t.Name, "", colourBlurple)
	none := []discordgo.MessageComponent{}
	switch {
	case t.ForumID == "":
		e.Description = "The tour has no forum. Use `/tour setup` to choose one."
		return e, none, nil
	case len(p.problems) > 0:
		e.Description = "Fix these problems with `/tour setup` or `/tour edit` first:\n- " + strings.Join(p.problems, "\n- ")
		e.Description = truncateEllipsis(e.Description, 4000)
		return e, none, nil
	case len(p.add)+len(p.change) == 0:
		e.Description = "There is nothing to post. All the shows are posted, and none of them changed."
		return e, none, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "In %s: **%d** new threads, **%d** changed threads.\n", channelMention(t.ForumID), len(p.add), len(p.change))
	list := func(title string, shows []db.TourShow) {
		if len(shows) == 0 {
			return
		}
		sb.WriteString("\n**" + title + "**\n")
		for i, s := range shows {
			if i == 15 {
				fmt.Fprintf(&sb, "…and %d more.\n", len(shows)-15)
				break
			}
			sb.WriteString("- " + p.text[s.ID].Title + "\n")
		}
	}
	list("New", p.add)
	list("Changed (the title and the post are updated)", p.change)
	e.Description = truncateEllipsis(sb.String(), 4000)
	if warn := b.tourForumProblems(t); len(warn) > 0 {
		e.Fields = append(e.Fields, &discordgo.MessageEmbedField{Name: "⚠️ Forum", Value: "- " + strings.Join(warn, "\n- ")})
	}
	first := p.add
	if len(first) == 0 {
		first = p.change
	}
	e.Fields = append(e.Fields, tourPreviewFields(p.text[first[0].ID])...)
	return e, confirmButtons("tourpost", c.UserID(), strconv.FormatInt(t.ID, 10)), nil
}

func (b *Bot) onTourPostConfirm(c *Ctx, extra []string) {
	if len(extra) < 1 {
		return
	}
	id, _ := strconv.ParseInt(extra[0], 10, 64)
	t := b.loadTour(c, id)
	if t == nil {
		return
	}
	if _, busy := b.tourBusy.LoadOrStore(t.ID, true); busy {
		c.Update([]*discordgo.MessageEmbed{errEmbed("Someone is posting this tour now. Try again when it is done.")}, []discordgo.MessageComponent{})
		return
	}
	defer b.tourBusy.Delete(t.ID)
	p, err := b.planTourPost(t)
	if err != nil {
		c.dbFailed("plan tour post", err)
		return
	}
	if t.ForumID == "" || len(p.problems) > 0 {
		c.Update([]*discordgo.MessageEmbed{errEmbed("The tour changed. Use `/tour post` again.")}, []discordgo.MessageComponent{})
		return
	}
	c.DeferUpdate()
	total, done := len(p.add)+len(p.change), 0
	var failed []string
	step := func(s db.TourShow, err error) {
		done++
		if err != nil {
			log.Printf("tour %d: show %s: %v", t.ID, s.Date, err)
			failed = append(failed, fmt.Sprintf("%s: %s", p.text[s.ID].Title, tourPostError(err)))
		}
		if done%5 == 0 && done < total {
			c.EditResponse(makeEmbed("🎸 Posting "+t.Name, fmt.Sprintf("%d of %d done…", done, total), colourBlurple))
		}
	}
	none := &discordgo.MessageAllowedMentions{}
	// The last show first, so that the next show is at the top of the forum.
	for _, s := range slices.Backward(p.add) {
		text := p.text[s.ID]
		th, err := b.s.ForumThreadStartComplex(t.ForumID, &discordgo.ThreadStart{Name: text.Title},
			&discordgo.MessageSend{Content: text.Post, AllowedMentions: none})
		if err == nil {
			err = b.db.SetTourShowPosted(s.ID, th.ID, text.Title, text.Post)
		}
		step(s, err)
	}
	for _, s := range p.change {
		step(s, b.updateTourThread(s, p.text[s.ID]))
	}
	e := makeEmbed("✅ "+t.Name, fmt.Sprintf("Made %d threads and updated %d threads in %s.", len(p.add), len(p.change), channelMention(t.ForumID)), colourGreen)
	if len(failed) > 0 {
		e.Color = colourOrange
		e.Description += fmt.Sprintf("\n\n**%d failed.** Use `/tour post` again to try them again:\n- %s", len(failed),
			truncateEllipsis(strings.Join(failed, "\n- "), 3500))
	}
	c.FinishUpdate(e)
}

// updateTourThread changes the title and the post of a posted show. An
// archived thread opens again; Discord archives it again later.
func (b *Bot) updateTourThread(s db.TourShow, text tourText) error {
	open := false
	if _, err := b.s.ChannelEditComplex(s.ThreadID, &discordgo.ChannelEdit{Name: text.Title, Archived: &open}); err != nil {
		return err
	}
	if text.Post != s.PostedPost {
		// The first message of a forum thread has the thread's ID.
		if _, err := b.s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: s.ThreadID, Channel: s.ThreadID,
			Content: &text.Post, AllowedMentions: &discordgo.MessageAllowedMentions{}}); err != nil {
			return err
		}
	}
	return b.db.SetTourShowPosted(s.ID, s.ThreadID, text.Title, text.Post)
}

func tourPostError(err error) string {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound {
		return "the thread or the forum was deleted"
	}
	return "Discord refused it (check the bot's log)"
}

// --- /tour edit ---

func (b *Bot) tourEditView(t *db.Tour) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	shows, err := b.db.TourShows(t.ID)
	if err != nil {
		return nil, nil, err
	}
	id := strconv.FormatInt(t.ID, 10)
	var sb strings.Builder
	for i, s := range shows {
		if i%tourShowsPerPage == 0 {
			fmt.Fprintf(&sb, "**Shows %d–%d**\n", i+1, min(i+tourShowsPerPage, len(shows)))
		}
		posted := ""
		if s.ThreadID != "" {
			posted = " · " + channelMention(s.ThreadID)
		}
		fmt.Fprintf(&sb, "`%s` %s%s\n", s.Date, truncateEllipsis(showSummary(s), 60), posted)
	}
	e := makeEmbed("✏️ "+t.Name, truncateEllipsis(sb.String(), 4000), colourBlurple)
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Empty a show's box to remove it from the tour (its thread stays). Changes go to Discord with /tour post."}
	rows := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "Edit name", Style: discordgo.PrimaryButton, CustomID: "tour:name:" + id}}}}
	var row []discordgo.MessageComponent
	for page := 0; page*tourShowsPerPage < len(shows) && page < tourPageButtons; page++ {
		from := page*tourShowsPerPage + 1
		row = append(row, discordgo.Button{Label: fmt.Sprintf("Shows %d–%d", from, min(from+tourShowsPerPage-1, len(shows))),
			Style: discordgo.SecondaryButton, CustomID: fmt.Sprintf("tour:page:%s:%d", id, page)})
		if len(row) == 5 {
			rows, row = append(rows, discordgo.ActionsRow{Components: row}), nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: row})
	}
	return e, rows, nil
}

// tourFieldOrder puts the usual columns first, then the others by name.
func tourFieldOrder(fields map[string]string) []string {
	first := []string{"venue", "city", "region", "link"}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		ia, ib := slices.Index(first, a), slices.Index(first, b)
		if ia < 0 {
			ia = len(first)
		}
		if ib < 0 {
			ib = len(first)
		}
		return cmp.Or(cmp.Compare(ia, ib), strings.Compare(a, b))
	})
	return keys
}

func showSummary(s db.TourShow) string {
	var parts []string
	for _, k := range []string{"venue", "city", "region"} {
		if v := s.Fields[k]; v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ", ")
}

// showText is the text of a show in the edit form: one "column: value"
// line for each column.
func showText(s db.TourShow) string {
	lines := []string{"date: " + s.Date}
	for _, k := range tourFieldOrder(s.Fields) {
		lines = append(lines, k+": "+s.Fields[k])
	}
	return strings.Join(lines, "\n")
}

// parseShowText reads the text of a show from the edit form.
func parseShowText(text string) (date string, fields map[string]string, err error) {
	fields = map[string]string{}
	for line := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		key := tourKey(k)
		v = strings.TrimSpace(v)
		switch {
		case !ok || key == "" || !tourKeyChars.MatchString(key):
			return "", nil, fmt.Errorf("`%s` is not a `column: value` line", truncateEllipsis(strings.TrimSpace(line), 60))
		case key == "tour":
			return "", nil, errors.New("use **Edit name** to change the tour name")
		case slices.Contains(tourDateTokens, key):
			return "", nil, fmt.Errorf("`%s` is a token that the bot makes from the date", key)
		case len([]rune(v)) > maxTourValue:
			return "", nil, fmt.Errorf("`%s` has more than %d characters", key, maxTourValue)
		case key == "date":
			date = v
		default:
			fields[key] = v
		}
	}
	if !validTourDate(date) {
		return "", nil, fmt.Errorf("the date `%s` is not a YYYY-MM-DD date", date)
	}
	return date, fields, nil
}

// --- components and forms ---

func (b *Bot) onTourComponent(c *Ctx) {
	if len(c.Args) < 2 {
		return
	}
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that."))
		return
	}
	if c.Args[0] == "pick" {
		vals := c.Values()
		if len(vals) != 1 {
			return
		}
		id, _ := strconv.ParseInt(vals[0], 10, 64)
		if t := b.loadTour(c, id); t != nil {
			c.Update(b.tourView(c, c.Args[1], t))
		}
		return
	}
	t := b.loadTour(c, tourID(c, 1))
	if t == nil {
		return
	}
	id := strconv.FormatInt(t.ID, 10)
	textInput := func(customID, label, value string, style discordgo.TextInputStyle, maxLen int) discordgo.MessageComponent {
		return discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID: customID, Label: label, Style: style, Value: value, Required: true, MaxLength: maxLen}}}
	}
	switch c.Args[0] {
	case "forum":
		vals := c.Values()
		if len(vals) != 1 {
			return
		}
		t.ForumID = vals[0]
		if err := b.db.UpdateTour(*t); err != nil {
			c.dbFailed("set tour forum", err)
			return
		}
		c.Update(b.tourView(c, "setup", t))
	case "tt":
		c.Modal("tourmodal:tt:"+id, "Thread title template",
			textInput("text", "Title template", t.TitleTemplate, discordgo.TextInputShort, maxThreadTitle*2))
	case "pt":
		c.Modal("tourmodal:pt:"+id, "Post template",
			textInput("text", "Post template", t.PostTemplate, discordgo.TextInputParagraph, maxTourTemplate))
	case "newforum":
		c.Modal("tourmodal:newforum:"+id, "New forum", textInput("name", "Forum name", truncate(t.Name, 100), discordgo.TextInputShort, 100))
	case "name":
		c.Modal("tourmodal:name:"+id, "Tour name", textInput("name", "Tour name", t.Name, discordgo.TextInputShort, 80))
	case "page":
		page, _ := strconv.Atoi(c.Args[2])
		shows, err := b.db.TourShows(t.ID)
		if err != nil {
			c.dbFailed("load tour shows", err)
			return
		}
		from := page * tourShowsPerPage
		if from >= len(shows) {
			c.Reply(true, errEmbed("The shows changed. Use `/tour edit` again."))
			return
		}
		var inputs []discordgo.MessageComponent
		for _, s := range shows[from:min(from+tourShowsPerPage, len(shows))] {
			label := truncateEllipsis(s.Date+" "+showSummary(s), 45)
			inputs = append(inputs, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
				CustomID: "s" + strconv.FormatInt(s.ID, 10), Label: label, Style: discordgo.TextInputParagraph,
				Value: showText(s), MaxLength: 4000}}})
		}
		c.Modal("tourmodal:shows:"+id, fmt.Sprintf("Shows %d–%d", from+1, from+len(inputs)), inputs...)
	}
}

func (b *Bot) onTourModal(c *Ctx) {
	if len(c.Args) < 2 {
		return
	}
	if !b.isMod(c.Member()) {
		c.Reply(true, errEmbed("You must be a moderator to use that."))
		return
	}
	t := b.loadTour(c, tourID(c, 1))
	if t == nil {
		return
	}
	switch c.Args[0] {
	case "tt", "pt":
		text := strings.TrimSpace(c.ModalValue("text"))
		if text == "" {
			c.Reply(true, errEmbed("The template is empty."))
			return
		}
		if c.Args[0] == "tt" {
			t.TitleTemplate = strings.Join(strings.Fields(text), " ")
		} else {
			t.PostTemplate = text
		}
		if err := b.db.UpdateTour(*t); err != nil {
			c.dbFailed("save tour template", err)
			return
		}
		c.Update(b.tourView(c, "setup", t))
	case "newforum":
		name := strings.TrimSpace(c.ModalValue("name"))
		if name == "" {
			c.Reply(true, errEmbed("The forum needs a name."))
			return
		}
		ch, err := b.s.GuildChannelCreateComplex(c.GuildID(), discordgo.GuildChannelCreateData{
			Name: name, Type: discordgo.ChannelTypeGuildForum, PermissionOverwrites: b.tourForumOverwrites(c.GuildID()),
		}, auditReason("Tour forum for "+t.Name))
		if err != nil {
			c.logErr("create tour forum", err)
			c.Reply(true, errEmbed("I could not make the forum. Check the bot's log."))
			return
		}
		t.ForumID = ch.ID
		if err := b.db.UpdateTour(*t); err != nil {
			c.dbFailed("set tour forum", err)
			return
		}
		c.Update(b.tourView(c, "setup", t))
	case "name":
		t.Name = strings.Join(strings.Fields(c.ModalValue("name")), " ")
		if t.Name == "" {
			c.Reply(true, errEmbed("The tour needs a name."))
			return
		}
		if err := b.db.UpdateTour(*t); errors.Is(err, db.ErrTourName) {
			c.Reply(true, errEmbed("There is already a tour with that name."))
			return
		} else if err != nil {
			c.dbFailed("rename tour", err)
			return
		}
		c.Update(b.tourView(c, "edit", t))
	case "shows":
		b.saveTourShows(c, t)
	}
}

// saveTourShows saves a page of the edit form. It checks all the shows
// before it saves one.
func (b *Bot) saveTourShows(c *Ctx, t *db.Tour) {
	shows, err := b.db.TourShows(t.ID)
	if err != nil {
		c.dbFailed("load tour shows", err)
		return
	}
	byID := map[int64]db.TourShow{}
	for _, s := range shows {
		byID[s.ID] = s
	}
	type change struct {
		id     int64
		date   string
		fields map[string]string
		remove bool
	}
	var changes []change
	dates := map[string]int64{}
	for _, s := range shows {
		dates[s.Date] = s.ID
	}
	var problems []string
	for key, text := range c.ModalTexts() {
		id, err := strconv.ParseInt(strings.TrimPrefix(key, "s"), 10, 64)
		old, ok := byID[id]
		if err != nil || !ok {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			changes = append(changes, change{id: id, remove: true})
			delete(dates, old.Date)
			continue
		}
		date, fields, err := parseShowText(text)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v.", old.Date, err))
			continue
		}
		if date != old.Date {
			delete(dates, old.Date)
		}
		changes = append(changes, change{id: id, date: date, fields: fields})
	}
	for _, ch := range changes {
		if ch.remove {
			continue
		}
		if other, ok := dates[ch.date]; ok && other != ch.id {
			problems = append(problems, fmt.Sprintf("Two shows have the date %s.", ch.date))
		}
		dates[ch.date] = ch.id
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		c.Reply(true, errEmbed("I did not save the changes:\n- "+strings.Join(problems, "\n- ")))
		return
	}
	// Free the old dates first, so that two shows can change dates.
	for _, ch := range changes {
		if !ch.remove && ch.date != byID[ch.id].Date {
			if err := b.db.UpdateTourShow(ch.id, "~"+strconv.FormatInt(ch.id, 10), byID[ch.id].Fields); err != nil {
				c.dbFailed("save tour show", err)
				return
			}
		}
	}
	for _, ch := range changes {
		if ch.remove {
			err = b.db.DeleteTourShow(ch.id)
		} else {
			err = b.db.UpdateTourShow(ch.id, ch.date, ch.fields)
		}
		if err != nil {
			c.dbFailed("save tour show", err)
			return
		}
	}
	c.Update(b.tourView(c, "edit", t))
}

// --- /tour help ---

func (b *Bot) cmdTourHelp(c *Ctx) {
	e := makeEmbed("🎸 Tour threads", "The bot makes one forum thread for each show. Members can reply in the threads, but only mods and the bot can make new threads.\n\n"+
		"**Steps**\n"+
		"1. `/tour upload` a CSV or JSON file of the shows.\n"+
		"2. `/tour setup`: choose the forum (or make a new one), and edit the thread title and post templates. The preview shows the next show.\n"+
		"3. `/tour post`: see what will change, then post. The bot makes the threads from the last show to the first, so the next show is at the top.\n"+
		"4. To change a tour later, upload a file again or use `/tour edit`. Then use `/tour post` again: it makes the new threads and updates the changed ones.",
		colourBlurple)
	e.Fields = []*discordgo.MessageEmbedField{
		{Name: "The file", Value: "- CSV with a header row, or a JSON array of objects (one for each show).\n" +
			"- `tour` and `date` must be in the file. The date must be `YYYY-MM-DD`, for example `2027-03-05`.\n" +
			"- Every other column is a token: a `link` column gives `{link}`. Column names can have letters, numbers, spaces, `-`, and `_` (a space is `_` in the token).\n" +
			"- One file can have more than one tour. The tour and the date find a show: an upload adds new dates and changes the ones that are there. Shows that are not in the file do not change.\n" +
			fmt.Sprintf("- No more than %d shows in a file, and %d in a tour.", maxTourRows, maxTourShows)},
		{Name: "Tokens", Value: "- `{tour}`: the tour name.\n" +
			"- `{date}`: `MM/DD` when `region` is a US state (or empty), else `DD/MM`.\n" +
			"- `{date_us}` (MM/DD), `{date_eu}` (DD/MM), `{date_iso}` (YYYY-MM-DD), `{weekday}` (Friday), `{month}` (March).\n" +
			"- `{date_local}`: the date in each reader's own format and language. Posts only: titles show it as code.\n" +
			"- `{any_column}`: the value from the file.\n" +
			"- An uppercase token gives uppercase text: `{CITY}` is `PORTLAND`, `{city}` is `Portland`."},
		{Name: "Defaults", Value: "Title: `" + defaultTourTitle + "`\nPost: `" + defaultTourPost + "`"},
		{Name: "Old tours", Value: "When the last show of a tour has passed, the tour does not show in the menus. The bot keeps its data."},
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "The attached file is a sample. Change it and upload it with /tour upload."}
	if err := c.ReplyFiles(true, e, &discordgo.File{Name: "tour-sample.csv", ContentType: "text/csv",
		Reader: strings.NewReader(tourSampleCSV)}); err != nil {
		c.logErr("tour help", err)
	}
}
