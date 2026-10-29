# HEALTH Bot

A moderation, engagement and community bot for the HEALTH Discord server. It is written in Go with [discordgo](https://github.com/bwmarrin/discordgo) and a local SQLite database, and it is one static binary with no runtime dependencies.

---

## Features

- **Slash commands only.** Every command is a slash command. Triggers are the one exception: they fire on `@BotName <trigger>`.
- **Moderation:** ban, tempban, kick, mute/unmute, purge, and a self-timeout for members.
- **Warnings** with a rap sheet, single-warning removal, and automatic arrest and kick thresholds.
- **Post images:** mods make an image of a post on X, to share it without a link.
- **Video downloads:** mods download a video from YouTube, X, or another site that [yt-dlp](https://github.com/yt-dlp/yt-dlp) supports, and get it in a private reply.
- **Comics:** everyone can draw the chat as a comic strip in the style of Microsoft Comic Chat: `/comic` for the last messages, or **Start comic here** to pick the messages first. Members choose their character with `/character`. *(Needs the Message Content option and the [comic art](#comic-art).)*
- **Speech clips:** mods make a voice message or an audio clip of a text with `/say`, in a preset voice, with a local [Echo-TTS](https://github.com/jordandare/echo-tts) worker on a GPU. *(Needs the [text to speech worker](#text-to-speech).)*
- **User notes:** private mod notes on members. Mods add a note with `/note add`, or right-click a message to add it to its author's notes.
- **Jail:** restrict a member to one channel for a set time, with sentence tracking, automatic release, and `/jail audit` to find channels the jail role can still see.
- **Tickets:** members open private mod-support tickets with a reaction; mods close, archive, list, note and search them.
- **Triggers:** mod-defined keyword responses, with random picks from several responses and a Carl-bot import.
- **Autoreacts:** the bot reacts with an emote when a word or phrase is said. *(Needs the [Message Content option](#message-content-features).)*
- **Reaction roles** built with a wizard.
- **Markov chat:** the bot learns how people talk in chosen channels and replies with its own mix of their words. It has a blocklist and tools to remove words. Its data is in a separate database file.
- **Curation (starboard):** messages with enough reactions are copied to a curated channel, with their pictures and files. *(Needs the [Message Content option](#message-content-features).)*
- **Audit logging:** messages, channels, threads, roles, voice, bans and invites.
- **Admin tools:** database backups (also daily), emoji/sticker export and import, a channel permissions report, permission templates, and channel archiving.
- **In-Discord configuration** with `/setup`. You do not need to edit `.env` for most settings.
- **Timers survive a restart:** tempban unbans, Member Of The Day removal, and jail releases are stored in the database.

---

## Requirements

- **Go 1.27 or later** to build. The server that runs the bot needs no Go and no C libraries: the SQLite driver is pure Go.
- A **Discord application with a bot user**, from the [Discord Developer Portal](https://discord.com/developers/applications).
- The bot invited to your server with the permissions in [Required bot permissions](#required-bot-permissions).
- Optional, for `/download`: **yt-dlp** and **ffmpeg** on the `PATH` of the server that runs the bot (see [Video downloads](#video-downloads)).

### Intents

The bot uses only the non-privileged gateway intents. It never requests **Server Members**: member lookups use single API calls instead.

**Message Content** is optional and off by default. See [Message content features](#message-content-features). Almost everything works without it. Triggers work because Discord always sends the text of a message that mentions the bot.

---

## Build

From the repository folder:

```bash
./configure
make
```

- `./configure` checks that Go is new enough, and asks for the platform to build for (the default is this machine; botden is `linux-amd64`) and the install folder (the default is `/usr/local/bin`). It saves the answers in `config.mk`, and shows them as the defaults the next time. `./configure --help` shows the options; `--yes` asks nothing.
- `make` builds `hcbot` and `hcbot-train`. `make help` lists all targets.

| Target | What it does |
|---|---|
| `make` | Builds both binaries for the configured platform. |
| `make linux` | Builds both for linux/amd64 into `dist/linux-amd64/`, whatever the configured platform is. |
| `make test` | `go test ./...` |
| `make check` | `gofmt`, `go vet`, the tests, and `golangci-lint` when it is installed. |
| `make install` | Copies both binaries to the install folder. Use `sudo`. It does not build, so Go never runs as root. |
| `make clean` | Removes the binaries, `dist/` and `build/`. `make distclean` also removes `config.mk`. |

Without `make`, build with Go directly:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o hcbot ./cmd/hcbot
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o hcbot ./cmd/hcbot
```

- `CGO_ENABLED=0` makes a static binary that runs on any Linux server of the same architecture.
- `-trimpath -ldflags="-s -w"` makes the binary smaller.

---

## Run

1. Put these files in one folder (the bot's **working folder**):
   - the `hcbot` binary,
   - a `.env` file (see [Configuration](#configuration)),
   - this `README.md` (the `/readme` command shows it).
2. Start the bot from that folder:

   ```bash
   ./hcbot
   ```

On the first start, the bot makes `hcbot.db` and `markov.db` in the working folder. A database from an older version, `health_bot.db`, is renamed to `hcbot.db` automatically when `hcbot.db` does not exist yet. Backups go into `backups/` in the same folder. After an update, the bot updates the database schema by itself.

The bot stops cleanly on `Ctrl+C` or `SIGTERM`. To run it as a service, see [Run as a systemd service](#run-as-a-systemd-service).

**Important:** run only one instance of the bot with the same token at a time.

---

## Configuration

### The `.env` file

Only the token must be in `.env`:

```ini
BOT_TOKEN=your-bot-token
GUILD_ID=123456789012345678

# Optional
SETUP_MODE=false
ENABLE_MESSAGE_CONTENT_FEATURES=false
MEDIA_CACHE_SIZE=300
MEDIA_CACHE_MAX_BYTES=8388608
GIPHY_API_KEY=
YTDLP_COOKIES=
TTS_URL=
TTS_TOKEN=
METRICS_ADDR=
```

| Key | Description |
|---|---|
| `BOT_TOKEN` | **Required.** The bot token from the Developer Portal. |
| `GUILD_ID` | Your server's ID. When set, slash commands sync to that server at once. When not set, the commands are global and can take up to an hour to appear. |
| `SETUP_MODE` | `true` turns on [setup mode](#setup-mode). |
| `ENABLE_MESSAGE_CONTENT_FEATURES` | `true` turns on the [message content features](#message-content-features). |
| `MEDIA_CACHE_SIZE` | How many messages' attachments the audit log keeps (default 300). |
| `MEDIA_CACHE_MAX_BYTES` | Largest attachment it keeps, in bytes (default 8 MiB). |
| `MARKOV_DB_PATH` | The database file for the [Markov chat](#markov-chat) (default `markov.db`). |
| `GIPHY_API_KEY` | The GIPHY API key for [Markov GIFs](#markov-chat). When it is not set, Markov GIFs are off. |
| `YTDLP_COOKIES` | A cookies file for yt-dlp, for [video downloads](#video-downloads) (for example `youtube-cookies.txt`, relative to the working folder). When it is not set, yt-dlp uses no cookies. |
| `TTS_URL` | The address of the [text to speech worker](#text-to-speech), for example `http://host.docker.internal:8765`. When it is not set, `/say` and `/voices` are not registered. |
| `TTS_TOKEN` | The secret token of the text to speech worker. It must be the same as `TTS_TOKEN` in the worker's `.env`. |
| `METRICS_ADDR` | Where the bot serves [Prometheus metrics](#metrics), for example `:9101`. When it is not set, the bot serves no metrics and opens no port. |
| `COMIC_ART_DIR` | The folder with the [comic art](#comic-art) (default `comic`, relative to the working folder). When it has no art, or Message Content is off, the comic commands are not registered. |

Keep `.env` private: the token gives full control of the bot, and the GIPHY key and the TTS token are also secret. If a token is ever shown in a log or a chat, reset it in the Developer Portal.

### `/setup`

Run `/setup` as a server administrator. It shows an ephemeral embed (only you see it) with a few settings on each page. The settings are in sections (Logs, Mods and tickets, Jail, Warnings, Community, Markov chat, File and memory limits, Video downloads, Links and files). Use the **Go to a section** menu at the top to go to a section, or the page buttons at the bottom. Channel, category and role settings use a dropdown. Number, emote and text settings use an **Edit** button. Each change is saved at once. The session expires after 5 minutes without use.

| Setting | What it is | Default |
|---|---|---|
| `MOD_LOG` | Channel for the mod log | — |
| `BIG_BROTHER` | Channel for the audit log | — |
| `SENSITIVE_LOG` | Channel for sensitive output (`/userinfo`). Falls back to `BIG_BROTHER` | — |
| `CURATED` | Channel for curated messages ¹ | — |
| `TICKET_CAT` | **Category** for new tickets | — |
| `MOD_SUPPORT` | Channel for the mod support embed | — |
| `MOD_ROLE` | The moderator role | — |
| `MOD_DM_PUNISHMENT` | Send a DM to a member when they are banned, kicked, muted, warned, or jailed. The DM shows the server, the action, the length, and the reason. It does not show the moderator. | on |
| `ON_THE_REAL` | Channel where autoreacts are off ¹ | — |
| `MOTD_ROLE` | Member Of The Day role | — |
| `CURATED_EMOTE` | Emote that counts toward curation ¹ | ⭐ |
| `CURATED_THRESHOLD` | Reactions needed to curate a message ¹ | 7 |
| `JAIL_ROLE` | The jail role | — |
| `JAIL_CHANNEL` | The only channel jailed members can speak in | — |
| `JAIL_PROTECTED_ROLES` | Up to 25 roles that jail never removes | (list) |
| `WARN_ARREST_THRESHOLD` | Warnings before an automatic arrest | 3 |
| `WARN_ARREST_DURATION` | Length of the automatic arrest, in minutes | 60 |
| `WARN_KICK_THRESHOLD` | Warnings before an automatic kick | 5 |
| `TRIGGER_IMPORT_MAX_MB` | Largest file for `/trigger batchimport`, in MB | 5 |
| `MEDIA_CACHE_TOTAL_MB` | Memory limit for all cached attachments, in MB ¹ | 200 |
| `ASSETS_IMPORT_MAX_MB` | Largest zip file for `/assets import`, in MB | 25 |
| `README_PATH` | File that `/readme` shows, relative to the working folder | `./README.md` |
| `PRIVACY_URL` | Privacy Policy link at the bottom of `/help`. Set to `none` to hide it. | the `PRIVACY.md` link in this repository |
| `TERMS_URL` | Terms of Service link at the bottom of `/help`. Set to `none` to hide it. | the `TERMS.md` link in this repository |
| `MARKOV_LEARN_CHANNELS` | Up to 25 channels the [Markov chat](#markov-chat) learns from | (list) |
| `MARKOV_REPLY_CHANNELS` | Up to 25 channels where the Markov chat replies | (list) |
| `MARKOV_REPLY_CHANCE` | Percent chance (0–100) that the Markov chat talks after a normal message in a reply channel. Decimals are permitted: `0.1` is about 1 in 1000 messages, `0.05` is about 1 in 2000. | 0 |
| `MARKOV_RANDOM_AS_REPLY` | Percent (0–100) of those random messages sent as a reply to the message. The rest are posted in the channel without a reply. | 50 |
| `MARKOV_COOLDOWN` | Seconds after a Markov message before the chat talks again in the same channel | 60 |
| `MARKOV_CANDIDATES` | How many replies the Markov chat makes before it picks one (1–20). The GPU worker (`TTS_URL`) picks. 1: no picking. See "Picking" below. | 8 |
| `MARKOV_PICK` | Which candidate the chat posts: `best` (the highest score) or `top3` (a random one of the best three, for more variety). A menu in `/setup`. | top3 |
| `MARKOV_MENTION_REPLY` | Who gets a reply to an @mention. **`always`:** everyone, also during the cooldown. **`cooldown`:** a mention gets a reply only when the cooldown is over. **`mods`:** only mods get a reply; other people get no reply to a mention. Mods always get a reply. (A menu in `/setup`.) This setting replaces `MARKOV_ALWAYS_REPLY_MENTION`: at start, the bot changes an old saved value (on → `always`, off → `cooldown`). | always |
| `MARKOV_GIF_CHANCE` | Percent chance (0–100) that a Markov message also gets a GIF from GIPHY. Decimals are permitted. Needs `GIPHY_API_KEY` in `.env`. | 0 |
| `MARKOV_GIF_COOLDOWN` | Seconds between GIPHY requests, for the full bot | 40 |
| `MARKOV_GIF_RATING` | Highest GIPHY rating for Markov GIFs: `g`, `pg`, `pg-13`, or `r` (a menu in `/setup`) | pg-13 |
| `MARKOV_IMPORT_MAX_MB` | Largest file for `/markov import`, in MB | 25 |
| `MARKOV_LOG_CHANNEL` | Mod-only channel where the Markov chat logs each message that it reads in a learn channel, and if it learned the message. Empty: no log. See "Learn log" below. | |
| `MARKOV_LOG_MODE` | Which messages go to `MARKOV_LOG_CHANNEL`: `all`, `skipped` (only the messages that it did not learn), or `off` (no log, and the channel stays set). A menu in `/setup`. | all |
| `DOWNLOAD_ENABLED` | Lets mods use `/download` and **Apps → Download video** | on |
| `DOWNLOAD_MAX_MB` | Largest video that `/download` sends, in MB. The server's boost level can make the limit smaller: 10 MB with no boost or level 1, 50 MB at level 2, 100 MB at level 3. | 100 |
| `TTS_ENABLED` | Lets mods use `/say` (needs `TTS_URL` in `.env`) | on |
| `TTS_CHANNELS` | Channels where `/say` works (up to 25). Empty: all channels. | |
| `TTS_MAX_CHARS` | Longest text for `/say`, in characters (at most 600) | 300 |
| `TTS_COOLDOWN` | Seconds that a member must wait between `/say` requests. 0: no wait. | 30 |
| `TTS_QUEUE_MAX` | Most `/say` clips of this bot that can wait in the queue, after the one that runs | 5 |
| `COMIC_ENABLED` | Let members make [comics](#comics) ¹ | on |
| `COMIC_CHANNELS` | Channels where comics work (up to 25). Empty: all channels ¹ | — |
| `COMIC_MAX_MESSAGES` | Most messages in one comic (2–20) ¹ | 10 |
| `COMIC_COOLDOWN` | Seconds that a member must wait between comics. 0: no wait. ¹ | 30 |

¹ Shown only when `ENABLE_MESSAGE_CONTENT_FEATURES` is on, because the feature needs it. The stored value is kept when the option is off.

Values are read in this order: **database (set with `/setup`) → `.env` → default**. Each setting also has a `.env` key; channel and role keys have a suffix, for example `MOD_LOG_CHANNEL_ID` and `MOD_ROLE_ID`.

### Setup mode

Use `SETUP_MODE=true` when the bot joins a server where another bot still does the same work. In setup mode:

- Only `/setup` and `/permsreport` work. Other commands give a short "setup mode" message.
- All automatic behavior is off: triggers, autoreacts, reaction roles, tickets, curation, the audit log, the jail release, the timers and the daily backup.
- The bot's status shows "🚧 setup mode".

Set it to `false` (or remove it) and restart the bot when you are ready to switch.

### Message content features

`ENABLE_MESSAGE_CONTENT_FEATURES=true` turns on:

- **autoreacts**, with the `/autoreact` commands,
- **curation**,
- the **message text** in the audit log (edits, deletes and bulk-delete transcripts),
- **media recovery** for deleted messages,
- **comics**, with `/comic`, **Start comic here**, and `/character` (they also need the [comic art](#comic-art)).

When the option is off, the `/autoreact` commands are not registered, curation is off completely, and `/setup` and `/help` do not show them. The data stays in the database, so everything works again when you turn the option on.

The bot then also requests the Message Content intent, so you must also turn on **Message Content Intent** under **Bot** in the Developer Portal. Restart the bot after a change.

Two important facts, from tests with the real Discord API:

1. **Discord decides access to message text from the portal switch**, not from the intent that the code requests. When the switch is on, Discord sends the text of every message to the bot, also when `ENABLE_MESSAGE_CONTENT_FEATURES` is off. The bot stores and shows message text only when the option is on.
2. **A forward needs access to the message text.** Discord refuses to forward a message whose text the bot cannot read (error 160014), except a message that mentions the bot. Curation copies the text of each curated message, so it also needs the option.

---

## Required bot permissions

In **OAuth2 → URL Generator**, select the `bot` and `applications.commands` scopes, and give at least these permissions:

- View Channels, Send Messages, Read Message History, Add Reactions, Embed Links, Attach Files
- Manage Messages *(purge, reaction clean-up)*
- Manage Roles *(jail, MOTD, reaction roles, permission overwrites)*
- Manage Channels *(tickets, archives, `/channelarchive`, `/jail setup`)*
- Manage Server *(invite logging and ❌ invite deletion)*
- Manage Expressions and Create Expressions *(`/assets`)*
- Send Voice Messages *(`/say` voice messages)*
- View Audit Log
- Kick Members, Ban Members, Moderate Members

The bot's role must be **above** every role it gives or removes (jail, MOTD, reaction roles).

---

## Commands

All commands are slash commands. `<angle brackets>` are required, `[square brackets]` are optional. **mod only** means the `MOD_ROLE` or the Administrator permission; **admin only** means the Administrator permission. Mod commands have the default permission Manage Messages, so Discord hides them from other members. You can change this under **Server Settings → Integrations**. The bot always does its own check too (`MOD_ROLE` or Administrator), so a changed setting there cannot give access to other members.

### General

| Command | Description |
|---|---|
| `/timeout [minutes]` | Mute yourself for a break (default 10, max 1440). |
| `@BotName <trigger>` | Posts a trigger's response. The mention must be the first thing in the message. |
| `@BotName <text>` | In a `MARKOV_REPLY_CHANNELS` channel, the [Markov chat](#markov-chat) replies. |
| `/help` | Shows the commands you can use. |

Trigger responses show `@everyone`, `@here` and role mentions but **do not ping** them. User mentions and the reply ping still work.

### Moderation

| Command | Access | Description |
|---|---|---|
| `/ban <member> [reason]` | Ban Members | Bans a member. |
| `/tempban <member> [duration] [reason]` | Ban Members | Bans for `duration` minutes (default 60), then unbans. The unban survives a restart. |
| `/kick <member> [reason]` | Kick Members | Kicks a member. |
| `/mute <member> [duration] [reason]` | Moderate Members | Times out a member (default 10 minutes). |
| `/unmute <member>` | Moderate Members | Removes a timeout. |
| `/purge [amount]` | Manage Messages | Deletes up to 100 recent messages (default 10). |
| `/motd <member>` | mod only | Gives the Member Of The Day role for 24 hours. |
| `/warn add <member> [reason]` | mod only | Warns a member. Arrests at `WARN_ARREST_THRESHOLD`, kicks at `WARN_KICK_THRESHOLD`. |
| `/warn history <member>` | mod only | Shows a member's warnings. Also `/rapsheet <member>`. |
| `/warn remove <id>` | mod only | Removes one warning. |
| `/warn clear <member>` | mod only | Removes all warnings of a member. |
| `/userinfo [member]` | mod only | Member info, posted to `SENSITIVE_LOG`. |
| `/serverinfo` | mod only | Server overview. |
| `/readme` | mod only | Shows this README in pages. |

When `MOD_DM_PUNISHMENT` is on, the bot sends a DM to the member after `/ban`, `/tempban`, `/kick`, `/mute`, `/warn add`, `/jail arrest`, and the automatic arrest and kick from warnings. The DM shows the server, the action, the length (with the end time), and the reason. It does not show the moderator. For a ban or a kick, the bot sends the DM before the action, because it cannot send a DM after the member leaves. If the action then fails, the bot deletes the DM. The mod reply and the mod log show if the DM was sent or failed. A failed DM (for example, the member does not accept DMs) does not stop the action.

### User notes

Mods keep private notes on members. The bot records who added each note and when, and who edited it last. All replies are ephemeral. Each add, edit, and delete is logged to `MOD_LOG`.

| Command | Access | Description |
|---|---|---|
| `/note add <user> <text>` | mod only | Adds a note to a member (up to 1500 characters). The member does not have to be in the server. |
| `/note list <user>` | mod only | Shows a member's notes, newest first, 5 on each page. Select a note to see all of it, then **Edit** or **Delete** it (Delete asks to confirm). |
| **Apps → Add to user notes** (right-click a message) | mod only | Adds the message to its author's notes: its text, the names of its files, a link to it, and the time it was sent. Use **Add comment** to add your own text. The saved message text does not change. |
| **Apps → View user notes** (right-click a member) | mod only | Same as `/note list`. |

The right-click command does not need the Message Content intent. Discord sends the text of the selected message in the interaction.

### Video downloads

| Command | Access | Description |
|---|---|---|
| `/download <url>` | mod only | Downloads the video at the link and sends it to you in an ephemeral reply (only you see it). |
| **Apps → Download video** (right-click a message) | mod only | Same as `/download`, for the first link in the message (or in its link preview). |

The bot runs [yt-dlp](https://github.com/yt-dlp/yt-dlp), so it can download from YouTube, X (Twitter), and the other sites that yt-dlp supports. yt-dlp's generic extractor is off, and the bot refuses links to local or private addresses. Only one download runs at a time; another download waits in a queue. Live streams are not supported.

**More than one video:** a link can give up to 10 files (Discord's limit for one message). For a post on X with more than one video or GIF, the bot sends all of the post's own videos and GIFs, but not the videos of a quoted post (unless the post has none of its own). A link to one of them (`…/video/2`) gives only that one. A YouTube link with a list (`watch?v=…&list=…`) gives only the video in the link. The reply lists each file with its size and quality. If the files together are larger than the upload limit, they come in more than one message. Photos are not downloaded.

If the best quality of a file is larger than the upload limit (`DOWNLOAD_MAX_MB`, or the server's boost limit), the bot tries 720p, 480p, 360p and 240p for that file only, and tells you that it sent a lower quality. The bot prefers H.264 video with AAC sound, because some Discord clients play only the sound of AV1 or VP9 video. A download stops after 12 minutes.

**GIFs on X:** X keeps a GIF as a short MP4 with no sound. The bot finds these by their address and uses ffmpeg to make a GIF again (480 px wide at 15 frames each second, then 320 px at 12, then 240 px at 10, until one fits the upload limit). If no GIF fits, or ffmpeg fails, the bot sends the MP4 and tells you why. GIFs from other sites stay MP4.

**Server setup:** install `yt-dlp` and `ffmpeg` (ffmpeg joins the video and audio). Use the newest yt-dlp release, not an old distribution package, and update it frequently (`sudo yt-dlp -U`), because sites change often. For YouTube, yt-dlp also needs a JavaScript runtime such as [Deno](https://deno.com). Some sites (YouTube, Vimeo) block requests from data center addresses, so downloads from them can fail on a hosted server.

**YouTube cookies:** on a hosted server, YouTube often refuses downloads ("Sign in to confirm you're not a bot"). Give yt-dlp the cookies of a signed-in account with `YTDLP_COOKIES` in `.env`. Use a spare account, because YouTube can block an account that downloads. To export the cookies, see the [yt-dlp instructions](https://github.com/yt-dlp/yt-dlp/wiki/Extractors#exporting-youtube-cookies). Put the file in the bot's working folder, make it private to the bot user (`chmod 600`), and restart the bot. yt-dlp saves changed cookies back to the file, so the bot user must be able to write it. The file is a password for that account: do not commit it or share it.

Use this command only for videos that you have the right to download and share. The bot keeps no copy: it deletes the file after the reply.

### Speech clips

| Command | Access | Description |
|---|---|---|
| `/say` | mod only | Opens a form with the text, a voice list, a **Format** list (**Voice message** or **Audio file**), and a channel list (the current channel is selected first). The bot posts the clip in that channel as a normal message: as a Discord voice message (without the text), or as an Ogg/Opus audio file with the voice name and the text. With `random`, the model makes a new voice for each clip. The form opens with your last voice and format. |
| **Apps → Respond with Voice** (right-click a message) | mod only | The same form, without the channel list. The bot posts the clip as a reply to the message. The reply does not notify the author. |
| **Apps → Speak this message** (right-click a message) | mod only | Speaks the text of the message, as a reply to it. The form has only the voice and the format. If the text is longer than `TTS_MAX_CHARS`, the form also has a text box with the text cut at a full word, so you can change it. Mentions become names, custom emoji their names, and links the word "link". The bot keeps the text in memory only while the form is open (at most 15 minutes). |
| `/voices` | mod only | Lists the voices. Admins also get a **Reload voices** button, which reads the voices folder again. |

The bot sends the text and the voice name to the [text to speech worker](#text-to-speech), and uses ffmpeg to make the WAV into Ogg/Opus. For a voice message, the bot also calculates the length and the waveform. If Discord refuses the voice message, the bot posts an audio file without the text. Only you see the queue position, the status, and the errors; the status goes away when the clip is posted. A mod can post only in a channel where they have View Channel and Send Messages, and `TTS_CHANNELS` applies to the channel where the clip goes. The bot needs Send Messages, Attach Files, and Send Voice Messages there.

The posts do not show who made them, so each clip is logged in `MOD_LOG`: who posted it, the format, the voice, the channel, the text, and a link to the clip (and to the message it answers). The voice list shows `random` and the first 24 voices (a Discord limit). The worker makes one clip at a time. A clip of up to 150 characters takes about 15 seconds, and a longer clip about 30 seconds. The other clips wait in a queue, and the reply shows their position while they wait. The queue is kept on disk, so a clip is not lost when the bot or the worker restarts. If the clip is not done 12 minutes after `/say`, the bot stops and shows an error. A clip has a maximum of about 30 seconds of speech; the model speaks faster to fit a long text.

Use only the voices of people who agree. Do not use `/say` to imitate a real person.

### Comics

| Command | Access | Description |
|---|---|---|
| `/comic [count]` | everyone | Draws the last `count` messages of the channel (1 to `COMIC_MAX_MESSAGES`, default 4) as a comic, and posts it. `@BotName comic [count]` does the same. |
| **Apps → Start comic here** (right-click a message) | everyone | Opens a private editor with that message and the next ones (at most 10). Click **Choose messages** and uncheck the messages that you do not want (a form with a checkbox for each message; the editor changes when you submit it), select a background, and use **Preview** to see the comic. Only you see the editor and the preview. **Post** sends the comic to the channel, with "Comic by" and your name (no ping). The editor closes after 14 minutes. |
| `/character [name]` | everyone | Without a name, shows your character. With a name, sets it (type to see the names). **Show all** shows all characters, and **Reset** goes back to an automatic character. |
| `/comicart reload` | admin only | Reads the comic art again, so that new [custom characters and backgrounds](#custom-comic-art) work without a restart. The reply lists the files that cannot be used, and why. If the art folder cannot be read, the old art stays. |

The layout follows [comicgen](https://github.com/iopred/comicgen), the comic library of the Septapus bot, so that the comic is readable when Discord shows it small in the chat: square panels (220 px) in 2 to 4 columns, and the cast ("A comic starring:", with the head of each character, the profile picture, and the server name) in the free cells at the start of the first row. Each panel has one speaker, or two short messages from two people at random, in the order of the messages. Each panel has a random zoom. The balloons are in the top half of the panel; long text gets smaller until it fits. Each character has a small profile picture at the bottom, so you can see who is who. Members who did not choose a character get one from their user ID, so it is usually the same one. Two automatic characters in one comic are never the same; a chosen character is always used. The words and emoji in a message select the face and the pose, as in Comic Chat: for example "lol" or 😂 laughs, ":(" is sad, all capitals shout, "hi" waves, and "you" points at the other person.

**Pictures:** the first picture of a message goes in its balloon, under the text: an uploaded image (PNG, JPEG, GIF, or WebP), a sticker, or a GIF or image link (for example from Tenor or GIPHY; the link is removed from the text). An animated picture shows its first frame. Link previews of web pages are not used. A picture marked as a spoiler is not shown: the balloon has a dark "spoiler" box. The bot gets each picture at a small size (at most 480 px wide) and keeps it only while it draws. If a picture cannot be fetched, the balloon says "🖼️ image".

Messages with no text and no picture, and system messages, are not drawn. Bot messages are drawn. Mentions become names, custom emoji their names, and links the word "link". A long message is cut. The bot reads the messages when it draws and keeps no text; the editor keeps only the message IDs. The bot needs Read Message History, Send Messages, and Attach Files.

### Post images

| Command | Access | Description |
|---|---|---|
| `/tweetshot <url> [public]` | mod only | Makes an image of a post on X, in a dark style. Only you see it, unless `public` is true. |
| **Apps → Screenshot post** (right-click a message) | mod only | Same as `/tweetshot`, for the first X link in the message (or in its link preview). Only you see it. |

The bot gets the post from X's embed data service (the service behind posts embedded on web pages), so it needs no X account. The image shows the avatar, the name and handle, the text with emojis, up to 4 photos or video frames, a quoted post, the date (UTC) and the reply and like counts. Sensitive media is blurred. The bot draws the image itself; it does not use a browser. A public reply has only the image, with no link to X. The ephemeral reply also has a link to the post. Links from `fxtwitter.com`, `vxtwitter.com`, `fixupx.com` and `fixvx.com` also work.

X does not show deleted, protected and age-limited posts in embeds, so the bot cannot make an image of them. The font has Latin, Greek and Cyrillic letters. Other scripts (for example Chinese, Japanese, Korean and Arabic) show as boxes, and right-to-left text is not correct. The embed data service is not an official API, so X can change it.

### Jail

| Command | Access | Description |
|---|---|---|
| `/jail arrest <member> [time] [reason]` | mod only | Jails a member. No `time` means no end. Also `/arrest`. |
| `/jail list` | mod only | Everyone in jail, with time served and time left. |
| `/jail amend <id> [time] [reason]` | mod only | Changes a sentence. Time is counted from the arrest; `indefinite` removes the end. |
| `/jail release <member>` | mod only | Releases a member early and adds a note to the rap sheet. |
| `/jail setup` | mod only | Applies the jail role's overwrites to every channel. |
| `/jail audit` | mod only | Finds channels the jail role can still see, and fixes them after you confirm. |

Time formats: `30m`, `2h`, `1d`, `1w`, combinations like `2h30m`, or a number of minutes (`45`).

An arrest removes the member's other roles, except `JAIL_PROTECTED_ROLES`, managed roles (bot roles, Server Booster), and roles above the bot. The removed roles come back on release. Run `/jail setup` once before the first arrest, and again after you make new channels. It adds to the jail role's current overwrites and does not replace them.

### Tickets

| Command | Access | Description |
|---|---|---|
| `/ticket close` | mod only | Closes the ticket in this channel. |
| `/ticket archive` | mod only | Moves this closed ticket into a `Ticket Archive N` category. |
| `/ticket archiveall` | mod only | Archives every closed ticket. |
| `/ticket list <member>` | mod only | Every ticket of a member. |
| `/ticket note <username-#> <note>` | mod only | Adds a note, for example `/ticket note alice-2 Resolved via DM.` |
| `/ticket notes <username-#>` | mod only | Shows a ticket's notes. |
| `/ticket search <query>` | mod only | Searches the notes. |

How tickets work:

1. A member reacts 📩 on the mod support embed (post it from `/setup`). The bot removes the reaction.
2. The bot makes `open-ticket-<username>-<n>` in `TICKET_CAT`. Only the member, `MOD_ROLE` and the bot can see it. If the member already has an open ticket, the bot sends a DM with a link instead.
3. 🔒 in the ticket (or `/ticket close`) removes the member's access and renames the channel to `closed-…`.
4. Archiving moves the channel into `Ticket Archive N` (at most 50 channels each; the bot makes the next one) with mod-only permissions. The channel and its history stay as they are.

### Triggers and autoreacts

| Command | Access | Description |
|---|---|---|
| `/trigger add <name> <value>` | mod only | Makes a trigger, or adds a response to it (the same as `addvalue`). |
| `/trigger addvalue <name> <value>` | mod only | Adds a response. With 2 or more, one is picked at random. |
| `/trigger removevalue <name> <value>` | mod only | Removes one response. |
| `/trigger delete <name>` | mod only | Removes a trigger. |
| `/trigger list` | mod only | All triggers. |
| `/trigger info <name>` | mod only | A trigger's responses. |
| `/trigger import` | mod only | Imports one Carl-bot tag with a popup. |
| `/trigger export` | mod only | All triggers as a JSON file. |
| `/trigger batchimport <file>` | mod only | Imports a JSON file from `/trigger export`, after you confirm. |
| `/autoreact add <emote> <phrase>` | mod only | Reacts with `emote` when `phrase` is said. |
| `/autoreact edit <emote> <phrase>` | mod only | Changes an autoreact's emote. |
| `/autoreact remove <phrase>` | mod only | Removes an autoreact. |
| `/autoreact list` | mod only | All autoreacts. |

A trigger with the same name as a command does not fire. The `/autoreact` commands exist only with `ENABLE_MESSAGE_CONTENT_FEATURES`. Autoreacts are off in `ON_THE_REAL`.

**Carl-bot import:** a `{rand:a,b,c}` or `{random:a~b~c}` block becomes several responses. A custom separator is possible: `{rand(|):a|b|c}`. If a block has both `,` and `~`, the bot asks which one to use. Other Carl-bot syntax (`{user}`, `{args}`, …) stays as text and is listed so you can fix it.

### Markov chat

| Command | Access | Description |
|---|---|---|
| `/markov status` | mod only | The size of the chain, its channels, the mode, and the GIF settings. |
| `/markov lookup <text>` | mod only | Shows if the chain contains a word or phrase. For one word, it shows each stored form (for example `cat` and `cat!`) and the words often before and after it. A button starts a forget. |
| `/markov forget <text>` | mod only | Removes a word, or a phrase, from the chain, after you confirm. |
| `/markov blockadd <text>` | mod only | Blocks a word or phrase. Then offers to remove it from the chain. A word that ends with `*` is a prefix: `word*` also blocks "words" and "wordy". Put at least 3 letters before the `*`. A short prefix can block normal words, so examine it with `/markov lookup` first. `forget` and `lookup` also accept a prefix. |
| `/markov blockremove <text>` | mod only | Removes a word or phrase from the blocklist. |
| `/markov blocklist` | mod only | All blocked words and phrases (hidden as spoilers). |
| `/markov import <file>` | mod only | Trains the chain from a `.txt` file (one message on each line) or a DiscordChatExporter `.json` export. |
| `/markov candidates <text>` | mod only | Shows the replies that the chat makes for a text, with their scores, best first, in an ephemeral reply. Use it to see how the picking works. |
| `/markov prune` | mod only | Removes the word steps seen only once, after you confirm. The database gets smaller. |
| `/markov reset` | admin only | Deletes the whole chain, after you confirm. The blocklist stays. |

See [Markov chat](#markov-chat) for how it learns.

### Reaction roles

| Command | Access | Description |
|---|---|---|
| `/reactionrole create` | mod only | Opens the wizard. |
| `/reactionrole edit <name>` | mod only | Opens the wizard for an existing reaction role. |
| `/reactionrole post <name>` | mod only | Posts or reposts a reaction role. |
| `/reactionrole list` | mod only | All reaction roles. |
| `/reactionrole delete <name>` | mod only | Deletes a reaction role and its message. |

In the wizard, set the name, the channel, the pairs (one `<emote> <role>` per line; the role can be a mention, an ID or a name) and the message text, then **Save**. The draft is kept for 10 minutes after the last change. A react gives the role; removing the react takes it away.

### Channel permissions

| Command | Access | Description |
|---|---|---|
| `/permsreport` | mod only | A text file that shows which channels are synced to their category and which have their own overwrites. Skips categories with "archive" and channels with "ticket" in the name. Works in setup mode. |
| `/permtemplate save <channel> <name>` | mod only | Saves a channel's overwrites as a template. |
| `/permtemplate list` | mod only | All templates. |
| `/permtemplate apply <name> <channel> <mode>` | mod only | Applies a template after you confirm. **Add to current permissions** merges the template into the channel's overwrites and removes nothing. **Replace all** makes the channel match the template exactly. |
| `/channelarchive [channel]` | mod only | Makes a copy of the channel as the new live channel, and moves the original into a `Channel Archive N` category. Each channel setting that used the original (for example `MOD_LOG` or `MARKOV_REPLY_CHANNELS`) is changed to the new channel, and the reply lists them. If `MOD_SUPPORT` changes, post the ticket embed again from `/setup`. |

A role or member in a template that no longer exists is skipped and listed.

### Administration

| Command | Access | Description |
|---|---|---|
| `/setup` | admin only | The settings UI. |
| `/db backup` | admin only | Makes a database backup and sends it as a file. |
| `/db list [filename]` | admin only | Lists backups, or sends one. |
| `/db delete <filename>` | admin only | Deletes a backup, after you confirm. |
| `/db configexport` | admin only | Sends the saved settings as a JSON file. |
| `/db configimport <file> <mode>` | admin only | Imports settings from a JSON file, after you confirm. The bot makes a backup first. |
| `/assets export` | admin only | All custom emojis and stickers as a zip file. |
| `/assets import <file>` | admin only | Imports a zip from `/assets export`. |
| `/botprofile avatar <file>` | admin only | Changes the bot's avatar everywhere, after you confirm. |
| `/botprofile banner <file>` | admin only | Changes the bot's profile banner everywhere, after you confirm. |

Backups are in `backups/`, named `hcbot_<UTC time>.db`. Backups from older versions (`health_bot_<UTC time>.db`) are still listed. They are safe copies made while the bot runs. The bot also makes a backup every day at 04:00 UTC, but only when the database changed. Old backups are not deleted automatically.

`/db configexport` and `/db configimport` let you repair the settings by hand. Export the settings, change or remove values in the JSON file, and import the file again. The file holds only the values saved in the database, not `.env` values or defaults. The `mode` option controls the import:

- `replace`: the file becomes the full set of saved settings. A key that you remove from the file is removed from the database. The setting then uses its `.env` value or default.
- `merge`: the bot adds and changes only the keys in the file. Other settings stay as they are.

The import refuses a file with a value that is not correct (for example, text in a number setting), and changes nothing. It shows what it will add, change and remove before you confirm, and warns about keys that the bot does not use.

`/botprofile` accepts PNG, JPEG, GIF and WebP images up to 8 MB, and shows a preview before you confirm. The change is logged in `MOD_LOG`. Discord allows only a few profile changes each hour. If you change the avatar or banner too often, the bot tells you how long to wait.

`/assets import` checks the free slots first (Discord counts static and animated emojis separately) and changes nothing if there is not enough room. For a name that already exists, you get a DM (or a private message in the channel, if your DMs are closed) with the old and the new image and **Replace** / **Keep Existing** buttons. Lottie and GIF stickers cannot be exported, because the bot API cannot upload them again.

---

## Automatic behavior

- **Curation** (only with `ENABLE_MESSAGE_CONTENT_FEATURES`): when a message gets `CURATED_THRESHOLD` × `CURATED_EMOTE`, the bot posts a copy in `CURATED`: the count and the channel above one embed with the author, the text, a **Jump to message** link, and the time. Each message is curated only once.
  - **Media:** the bot uploads the files, stickers, and embed images again (Discord's links to them expire), up to the server's upload limit. Up to 4 images show in the embed as a gallery. Spoiler images and other files (for example, videos) are attached under it. A file that is too large or cannot be read is named in the embed, with "see the original". If Discord refuses the files, the bot posts the embed without them.
  - **Embeds:** the text of embeds from bots (author, title, description, fields, footer) is copied into the embed, as a quote. Link previews (YouTube, X, GIFs, websites) are not copied: their links go in the text above the embed, so that Discord shows its own previews, which can play.
  - A reply shows "Replying to" and the name. The copy pings nobody. If the author edits or deletes the original, the copy does not change.
- **Invites:** new invites are logged in `MOD_LOG`. React ❌ on the log entry to delete the invite. This also works after a restart.
- **Bans and unbans** from any source are logged in `MOD_LOG`.
- **Timers:** tempban unbans, MOTD removals and jail releases are checked every 30 seconds and survive a restart.

## Markov chat

The bot learns how people talk in the `MARKOV_LEARN_CHANNELS` channels, and replies in the `MARKOV_REPLY_CHANNELS` channels. Both lists also include the threads in those channels. When a list is empty, that part is off.

**Learning:**

- **Without `ENABLE_MESSAGE_CONTENT_FEATURES`:** the bot learns only from messages that contain `@BotName`. Use `/markov import` or `hcbot-train` to train it with more text.
- **With `ENABLE_MESSAGE_CONTENT_FEATURES`:** the bot learns from all messages in the learn channels.

**Talking** (in the reply channels):

- **Mentions:** the bot replies to `@BotName <text>`. With Message Content on or off, it tries to start from a word of your message.
- **Random talk:** after a normal message, the bot talks with a chance of `MARKOV_REPLY_CHANCE` percent. For a busy server, use a decimal value such as `0.1` (about 1 in 1000 messages). `MARKOV_RANDOM_AS_REPLY` percent of these messages are replies to the message; the others are posted in the channel. This also works without Message Content, but then the bot cannot read the message, so it starts at a random place in the chain.
- **Cooldown:** after the bot talks in a channel, it waits `MARKOV_COOLDOWN` seconds before random talk in that channel again. Each channel has its own timer, and a restart resets the timers. With `MARKOV_MENTION_REPLY` set to `always`, mentions always get a reply; with `cooldown`, mentions also wait for the cooldown; with `mods`, only mods get a reply to a mention. Mentions from mods (`MOD_ROLE`, the Administrator permission, or the server owner) always get a reply. Each Markov message starts the cooldown, also a reply to a mention.

**Picking:** with the GPU worker (`TTS_URL`, see [Text to speech](#text-to-speech)), the chat makes `MARKOV_CANDIDATES` different replies for each message. A small language model ([Qwen 2.5 0.5B Instruct](https://huggingface.co/Qwen/Qwen2.5-0.5B-Instruct), Apache 2.0) gives each one a score: how probable the model finds it as an answer to the message. Thus, a reply that is fluent and fits the message gets a high score. When the bot cannot read the message (a random message without the Message Content option), the score is how natural the reply is as a chat message. The bot posts the best one, or a random one of the best three (`MARKOV_PICK`). If the worker does not answer in 3 seconds, the bot posts the first candidate. The text goes only to the worker on the bot's own server, which does not log or keep it.

**GIFs:** with `GIPHY_API_KEY` in `.env`, each Markov message has a chance of `MARKOV_GIF_CHANCE` percent to get a GIF. The bot uses the text of its message (at most 50 characters, cut at a full word) as a GIPHY search, and puts the first result in an embed on the same message. `MARKOV_GIF_RATING` is the highest rating. `MARKOV_GIF_COOLDOWN` is the time between GIPHY requests for the full bot, because the API key has an hourly limit. For example, a key with 100 requests each hour needs a cooldown of at least 36 seconds. The cooldown starts at each request, also a failed request. If GIPHY sends an error, finds nothing, or does not answer in 3 seconds, the bot posts the message without a GIF. The bot sends the Markov text to GIPHY, but not who wrote the words.

A trigger wins: `@BotName <trigger>` posts the trigger, not a Markov reply.

**What it learns:** the words of each message, in lowercase, with the pairs of words that come before each word. It does not keep who said a message, or the full message. A user mention is learned as that user's name (the global name or the username, without `@`), and a leading `@` is removed from other words, so a reply never pings anyone. It leaves out role and channel mentions, `@everyone`, `@here`, links, and words longer than 64 characters. It ignores a full message that contains a blocked word or phrase. Replies cannot ping anyone.

**Blocklist:** a blocked entry matches whole words, without case and without punctuation at either end. For example, `cat` blocks `Cat!` but not `cats`. A word that ends with `*` is a prefix: `cat*` blocks `cat`, `cats`, and `catch`. A phrase blocks those words in that sequence. The blocklist is in `hcbot.db`, so it stays when you delete the chain.

**Learn log:** set `MARKOV_LOG_CHANNEL` to a channel that only mods can see. For each message that the bot reads in a learn channel, it posts the result: **Learned** (with the number of words), or **Not learned** and the reason (a blocked entry, no words, or a full learn queue). The result shows the author, the channel, a jump link, and the message text (at most 1000 characters; attachments are not copied). Without Message Content, the bot reads only messages that mention it, so it logs only those. With Message Content on and `MARKOV_LOG_MODE` set to `all`, the bot posts one message for each message in the learn channels; on a busy server, use `skipped`. Set `MARKOV_LOG_MODE` to `off` to stop the log.

**Remove data:** `/markov forget` removes a word or phrase. `/markov reset` removes everything. You can also stop the bot, delete `markov.db`, `markov.db-wal` and `markov.db-shm`, and start the bot again. **Do not delete the file while the bot runs.**

**Size:** each word step uses about 40–60 bytes. A busy server can make 100–300 MB in some months. `/markov status` shows the size, and `/markov prune` makes it smaller. `markov.db` is not in the daily backup.

### Train with the `hcbot-train` script

```bash
make                  # builds hcbot-train with hcbot
./hcbot-train [-db markov.db] [-maindb hcbot.db] file...
```

Run it in the bot's working folder. Each file is plain text (one message on each line) or a `.json` export from [DiscordChatExporter](https://github.com/Tyrrrz/DiscordChatExporter). Bot messages in an export are skipped. The script uses the blocklist from `hcbot.db` and only reads that file. You can run it while the bot runs.

## Audit logging

These events go to `BIG_BROTHER`:

| Area | Events |
|---|---|
| Messages | Edits, deletes and bulk deletes. The text and recovered media only with `ENABLE_MESSAGE_CONTENT_FEATURES`. |
| Channels | Created, deleted, renamed, moved, topic, NSFW, slowmode and permission changes. |
| Threads | Created, deleted, renamed, archived, locked, slowmode changes. |
| Roles | Created, deleted, renamed, colour, position, hoist, mentionable, and each permission granted or revoked. |
| Voice | Join, leave, move, mute, deafen, stream and camera changes. |

The bot keeps the last 1000 messages in memory. A delete of an older message is logged as **"Old Message Deleted"** with its channel and send time; Discord does not tell who wrote it. Member join, leave, nickname and role changes are not logged, because they need the Server Members intent.

---

## Run as a systemd service

Each bot is an **instance**: a systemd service with its own name, user, working folder, `.env` and token. All instances run the same binary from the install folder (`/usr/local/bin`). The bot reads `.env`, `hcbot.db`, `markov.db`, `backups/` and `README.md` from its **working folder**, not from the folder of the binary.

`deploy/systemd/hcbot.service.in` is the template for the unit files. Do not install it directly.

### Set up an instance

On the server, in the repository folder:

```bash
# 1. Build and install the binaries.
./configure && make && sudo make install

# 2. Make a user for the bot (skip this if the user exists).
sudo useradd --system --home-dir /opt/healthbot --shell /usr/sbin/nologin hcbot

# 3. Set up the instance. It asks for the user, the group and the working
#    folder, and makes build/healthbot.service.
make instance NAME=healthbot

# 4. Install the folder and the unit, then add the .env and start it.
sudo make install-instance NAME=healthbot
sudo install -o hcbot -g hcbot -m 0600 .env /opt/healthbot/.env
sudo systemctl enable --now healthbot
```

- `make instance NAME=x` saves the answers in `instances/x.conf`. The next run shows them as the defaults. To ask nothing, give them: `make instance NAME=x BOTUSER=u BOTGROUP=g RUNDIR=/path </dev/null`.
- `make services` makes every `build/<name>.service` again, for example after a change to the template or to the install folder.
- `sudo make install-instance NAME=x` stops if the user or the group does not exist. It makes the working folder (owner: the bot user, mode 0700) when it does not exist, copies `README.md` into it, installs `/etc/systemd/system/x.service`, and runs `systemctl daemon-reload`. When the unit already exists and is different, it shows the difference and asks first (`FORCE=1` replaces it without a question). It does not start, stop or enable the service.

**Update the bot:** build and install again, then restart each instance:

```bash
make && sudo make install
sudo systemctl restart healthbot
```

A new binary changes all instances at their next restart.

### A folder in /home

`ProtectHome=yes` blocks all of `/home`, so a service with its folder there fails with `203/EXEC`. When the working folder is in `/home`, the generated unit uses these lines instead, so the bot sees only its own folder in `/home`:

```ini
ProtectHome=tmpfs
TemporaryFileSystem=/home/botden:mode=0755
BindPaths=/home/botden/healthbot
```

`TemporaryFileSystem` is necessary because `UMask=0077` would make the parent folder root-only, and the service would fail with `200/CHDIR`. For a deeper folder, such as `/home/botden/src/testbot`, the unit gets one `TemporaryFileSystem` line for each folder between: `/home/botden` and `/home/botden/src`. Keep `.env` and the databases private: `chmod 600`.

### Multiple instances

Repeat `make instance` and `sudo make install-instance` with a new name, a new working folder and a `.env` with another bot token. The instances share nothing but the binary. Each has its own databases, backups and log:

```bash
make instance NAME=otherbot RUNDIR=/opt/otherbot
sudo make install-instance NAME=otherbot
sudo install -o hcbot -g hcbot -m 0600 other.env /opt/otherbot/.env
sudo systemctl enable --now otherbot
journalctl -u otherbot -f
```

### Control and logs

Use the instance name, for example `healthbot`:

```bash
systemctl status healthbot        # state and the last log lines
sudo systemctl stop healthbot     # stop
sudo systemctl start healthbot    # start
sudo systemctl restart healthbot  # restart (for example, after a change to .env)

journalctl -u healthbot -f             # follow the log
journalctl -u healthbot --since today  # today's log
journalctl -u healthbot -p warning     # errors and warnings only
```

Under systemd, the bot writes its log lines without its own timestamp, because the journal adds one. The journal keeps the log according to its own limits (`/etc/systemd/journald.conf`, for example `SystemMaxUse=`).

---

## Run on TrueNAS (or other Docker hosts)

Each push to `main` builds a container image, `ghcr.io/binpas/hcbot` (linux/amd64), with GitHub Actions (`.github/workflows/image.yml`). The image has `hcbot`, `hcbot-train`, and the [video download](#video-downloads) tools: `yt-dlp`, `deno`, `ffmpeg`, and `ffprobe`. It has no secrets and no data. The tags are `latest` (the newest `main`) and `sha-<commit>` (one for each build, to go back to an older image).

The bot runs as UID and GID 568 (the TrueNAS `apps` user) in the folder `/data`. Mount the bot folder there. It holds `.env`, `hcbot.db`, `markov.db`, `backups/`, and the cookies file.

The image package on GHCR is private. Give TrueNAS a login for it once: make a GitHub personal access token (classic) with only the `read:packages` scope. Then add it in **Apps > Configuration > Manage Container Image Registries**, with registry `ghcr.io`, your GitHub user name, and the token as the password. Do not use your GitHub password.

1. Make a dataset for the instance, for example `POOL/apps/hcbot`. Make it belong to `apps` (568:568) with mode 0700.
2. Put the `.env` file in it (mode 0600). If you move an instance from another server, stop it there first. Then copy its `.env`, `hcbot.db`, `markov.db`, and cookies file. **Do not run the same bot token in two places at the same time.** If you do, each command gets two replies.
3. In TrueNAS, go to **Apps > Discover Apps > ⋮ > Install via YAML**. Paste `deploy/truenas/compose.yaml`. Change the container name and the host folder (`/mnt/POOL/...`).
4. Look at the app logs. You must see "Logged in as" and "Slash commands synced".

To update, use **Update** on the app (it gets the new `latest`), or set a `sha-<commit>` tag in the YAML. To update a download tool, change its version and checksum in the `Dockerfile`. The `yt-dlp` workflow (`.github/workflows/yt-dlp.yml`) does this for yt-dlp each Monday: it checks the release signature with the key in `deploy/yt-dlp-signing-key.asc`, commits the new version to `main`, and builds a new image. You must still update the app to use it.

The container has the same protection as the systemd unit: a read-only root file system, no Linux capabilities, `no-new-privileges`, and limits for memory, CPU, and processes. `/tmp` is a `tmpfs` (memory) of 2 GB for `/download`. The bot opens no ports, except the [metrics](#metrics) port when `METRICS_ADDR` is set.

To run `hcbot-train` in the container, use the app shell, or `docker exec -it hcbot hcbot-train ...`.

### Text to speech

`/say` needs a text to speech worker: `tts/server.py`, which runs [Echo-TTS](https://github.com/jordandare/echo-tts) on an NVIDIA GPU with at least 11 GB of video memory. The model uses about 6.3 GB when it is loaded, and up to 8.7 GB while it makes a clip. It stays loaded. The worker makes one clip at a time, in the order the clips come (from all bots that use it). It keeps the queue in `queue/` in its data folder, so the clips stay after a restart. A job file is deleted when the bot has the clip, or 30 minutes after the clip is done. The worker does not log the text, but it can save each clip with its text for the operator (see **Dashboard and saved clips** below).

The same worker also does the [Markov picking](#markov-chat), with a second, small model (about 1 GB more video memory). A ranking does not wait for a clip. `RANK_ENABLED=0` in the worker's `.env` turns the ranking model off.

The image is large (about 8 GB, most of it PyTorch and CUDA), so it is built on the GPU host and is not put on GHCR. The Echo-TTS code and the model have fixed versions (in `tts/Dockerfile` and `tts/server.py`). The model (about 6 GB) downloads into the data folder at the first start.

**Licenses:** the Echo-TTS code is MIT. The model weights, and each clip that the model makes, are [CC BY-NC-SA 4.0](https://creativecommons.org/licenses/by-nc-sa/4.0/): no commercial use. Do not use the model to imitate a real person without consent.

1. In TrueNAS, go to **Apps > Configuration > Settings** and turn on **Install NVIDIA Drivers**. `nvidia-smi` must show the GPU.
2. Make a dataset for the worker, for example `POOL/apps/tts`, owner `apps` (568:568), mode 0700. Put a `.env` in it (mode 0600) with `TTS_TOKEN=` and a long random value (`openssl rand -hex 32`).
3. Make a dataset for the voices, for example `POOL/apps/tts-voices`. Give the `apps` user read access. To add voices from your PC, share it with SMB.
4. Build the image on the host: `docker build -t hcbot-tts:local tts/`.
5. Optional: make a dataset for the saved clips, for example `POOL/apps/tts-clips`. Give the `apps` user write access, and share it with SMB. Add `DASHBOARD_TOKEN=` and a random value to the worker's `.env`.
6. Install `deploy/truenas/tts-compose.yaml` with **Install via YAML**. Change the folders and the API port address (the `docker0` address: `ip -4 addr show docker0`). The API port is not open on the LAN.
7. In the `.env` of each bot instance, set `TTS_URL=http://host.docker.internal:8765` and the same `TTS_TOKEN`. The bot app needs the `extra_hosts` line from `compose.yaml`, with the same `docker0` address (not `host-gateway`, which can give an IPv6 address). Restart the bot.

**Voices:** put one audio file for each voice in the voices folder (WAV, MP3, FLAC, OGG, or M4A). The file name is the voice name: use `a`–`z`, `0`–`9`, `-`, and `_` (at most 32 characters). A good clip is 5–10 seconds of one speaker, with clear speech and no music or noise. You do not need a transcript. Then use **Reload voices** in `/voices`, or restart the worker. If a voice sometimes changes into a different voice, put `<name>.json` next to the clip with `{"force_speaker": 1.5}` (try 1.1 to 1.5; a high value can make noise).

**Dashboard and saved clips:** the worker has a web page for the operator on port 8766 (`http://<host>:8766/`). It shows the running clip, the queue, and the last 50 clips with their text, voice, and times, and counts since the worker started. You can play or download each recent clip. The **Saved clips** tab shows the clips in `CLIPS_DIR` by day (UTC), with their text. You can search the text, filter by voice, and play or download each clip. The page does not change or delete clips. It does not show the Markov rankings. The list is only in memory, so it is empty after a restart. When `DASHBOARD_TOKEN` is set, the browser asks for a login: type the token as the password (any user name). The port is open on the LAN, so set the token if other people use your network.

When `CLIPS_DIR` is set (`/clips` in the compose file), the worker saves each clip as `<date>/<time>-<id>_<voice>_<text>.ogg` (Ogg/Opus, the same format that the bot posts), with a `.json` next to it that has the full text. The date and time are UTC. `CLIPS_KEEP_DAYS` in the worker's `.env` deletes the date folders that are older than that number of days (the default, 0, keeps all clips). `DASHBOARD_PORT=0` turns the page off.

If the worker stops, `/say` gives an error, and the rest of the bot works as usual.

### Comic art

The comic art is not in the repository. `deploy/comic-art.sh` downloads it into a folder, at a fixed version: the Microsoft Comic Chat characters (by Jim Woodring) and the backgrounds with Microsoft's copyright, from Microsoft's open-source release under the MIT license, as sprite sheets made by [comic-chat-web](https://github.com/remsky/comic-chat-web). The license files go in the folder too.

```bash
deploy/comic-art.sh /mnt/POOL/apps/hcbot/comic   # the bot folder is /data in the container
chown -R 568:568 /mnt/POOL/apps/hcbot/comic
```

The default `COMIC_ART_DIR` is `comic` in the working folder (`/data/comic` in the container). The folder has `avatars/manifest.json` with the sprite sheets, and `backgrounds/manifest.json` with the images, in the format of comic-chat-web. After a change, use `/comicart reload` or restart the bot.

### Custom comic art

Add your own characters and backgrounds in `custom/` in the art folder (for example `\\blade\hcbot\comic\custom` through SMB). `deploy/comic-art.sh` never changes this folder. Then use `/comicart reload`. The reply (and the log) lists each file that the bot cannot use, and why.

```
custom/characters/bob/neutral.png      required
custom/characters/bob/happy.png        optional poses (see the table)
custom/characters/bob/icon.png         optional
custom/characters/bob/character.json   optional: {"face": [95, 60], "users": ["123456789012345678"]}
custom/backgrounds/park.png            or .jpg
```

**Names:** the folder name is the character name, and the file name is the background name: 1 to 32 of `a`–`z`, `0`–`9`, `-` and `_` (capital letters become small). A name that a character or background already has is not used.

**Poses:** one PNG file for each pose. Only `neutral.png` is required. When a message needs a pose that the character does not have, the bot uses the nearest emotion, or `neutral`.

| File | Used when the message… |
|---|---|
| `neutral.png` | has no emotion (required) |
| `happy.png`, `laugh.png`, `coy.png`, `bored.png`, `scared.png`, `sad.png`, `angry.png`, `shout.png` | shows that emotion (for example "lol" or 😂 laughs, ":(" is sad, all capitals shout) |
| `wave.png` | starts with "hi", "hello", "bye"… |
| `point.png` | points at the other person ("you", "are you"…) |
| `self.png` | points at the speaker ("I", "I'm"…) |
| `shrug.png` | says "idk", "dunno", 🤷… |

**Required sizes and format:**

| Art | Format | Size | Recommended |
|---|---|---|---|
| A pose | PNG with a transparent background | 40–1000 px wide, 100–1000 px high, at most 8 MB | The whole body, about **400 px high** and 150–250 px wide, all poses the same size |
| `icon.png` | PNG with a transparent background | 16–400 px each side | A square head, 40–80 px |
| A background | PNG or JPEG | 160–2000 px each side, at most 8 MB | **Square**, 320–640 px |

How the art is drawn, so that it looks right:

- **Characters face right.** In a panel with two speakers, the bot mirrors the second one.
- **The comic draws a pose at 0.27 to 0.54 times its size** (a random zoom for each panel), from the middle of the panel down. A 400 px body is then 110–220 px high: the whole body at zoom 1, and the top half at zoom 2. Put the head at the top, and make the face clear at a small size. A pose that is much smaller or larger than 400 px high looks small or large next to the other characters.
- **Thick black outlines and a solid fill** read best on the busy backgrounds. Keep the background of each pose transparent; a pose without a transparent background is used, but it covers the panel (the bot lists it as a problem).
- **The face point** is where the balloon tail points, in `neutral.png` pixels: `{"face": [x, y]}` in `character.json`. Without it, the bot uses the middle of the top fifth of `neutral.png`. For poses of another size, the point is scaled.
- **The icon** is shown in the cast list and in `/character`. Without `icon.png`, the bot cuts a square around the face point of `neutral.png`.
- **A background** is stretched to the square panel, and zoomed up to 2× into the left part, at the middle height. Keep the important parts away from the right edge. The characters cover the bottom half, and the balloons the top half.
**Private characters:** add a `"users"` list of Discord user IDs to `character.json` (in Discord, turn on Developer Mode, then right-click a user and select **Copy User ID**). Only those users can choose the character with `/character`, and only they see it in the autocomplete and in **Show all**. For other users, `/character` says that there is no character with that name. Comics never give a private character at random. Everyone who reads a comic sees the character in it. When you remove a user from the list (then `/comicart reload`), comics give that user a random character again; their choice comes back when you add them again. If `character.json` has an error, or no ID in `users` is correct, the character is not used, so that it cannot become public by mistake. At least one character must be public.

- Only use art that you made or may use. The art goes only to the servers where the bot posts the comics.

---

## Metrics

With `METRICS_ADDR` (for example `:9101`), the bot serves [Prometheus](https://prometheus.io/) metrics at `/metrics`. Open the port only on your local network: the metrics have no user data, but they show what the bot does. The labels hold only the names of commands, features, and results, never user, server, or channel names or IDs.

| Metric | What it shows |
|---|---|
| `hcbot_build_info{version}` | The running commit. |
| `hcbot_gateway_connected`, `hcbot_gateway_latency_seconds`, `hcbot_gateway_events_total{event}` | The connection to Discord: connected (1 or 0), the heartbeat time, and the connects, disconnects, and resumes. |
| `hcbot_interactions_total{kind,name,result}` | Commands, buttons, menus, forms, and autocompletes. `result` is `answered`, `no_answer`, or `panic`. |
| `hcbot_interaction_answer_seconds{kind,name}` | The time until the first answer. Discord fails an interaction after 3 seconds. |
| `hcbot_handler_seconds{handler}` | The time of the message and reaction handlers. |
| `hcbot_discord_requests_total{host,route,method,status}`, `hcbot_discord_request_seconds{host,route}` | The HTTP requests to Discord's API (`api`), its file server (`cdn`), and other hosts (`other`). The routes have no IDs. A `429` status is a rate limit. |
| `hcbot_comics_total{source,result}`, `hcbot_comic_seconds` | Comics from `/comic` (`slash`), `@Bot comic` (`mention`), and the editor (`preview`, `post`), and the draw time. |
| `hcbot_curated_total{result}`, `hcbot_curated_files_total{result}` | Curated posts, and their copied files (`copied`, `too_large`, `failed`). |
| `hcbot_tts_jobs_total{result}`, `hcbot_tts_job_seconds` | `/say` clips, and the time from the request to the posted clip. |
| `hcbot_downloads_total{result}`, `hcbot_download_bytes` | `/download` requests, and the size of the posted files. |
| `hcbot_markov_replies_total{kind}`, `hcbot_markov_rank_seconds` | Markov chat replies (`mention`, `chance`), and the time of a rank request. |
| `hcbot_triggers_total`, `hcbot_autoreacts_total` | Triggers that answered, and autoreact emotes. |
| `hcbot_db_bytes`, `hcbot_markov_db_bytes` | The size of the databases. |
| `hcbot_backups_total{result}`, `hcbot_last_backup_timestamp_seconds` | Database backups. Use the time for an alert when backups stop. |
| `hcbot_nick_cache_entries`, `hcbot_nick_lookups_total{result}`, `hcbot_message_cache_entries`, `hcbot_media_cache_bytes` | The caches. |
| `go_*`, `process_*` | Go memory, goroutines, garbage collection, CPU, and open files. |

The [text to speech worker](#text-to-speech) serves its own metrics at `/metrics` on its dashboard port (8766), with no login (they have no text): `hcbot_tts_jobs{state}` (queued and running), `hcbot_tts_clips_total{result}`, `hcbot_tts_generate_seconds` (the time to make a clip), `hcbot_tts_rank_requests_total{result}`, `hcbot_tts_rank_seconds`, `hcbot_tts_voices`, and `hcbot_tts_gpu_memory_bytes{kind}`.

`deploy/monitoring/` has a Prometheus and Grafana app for a TrueNAS host on the same network (`compose.yaml`, with the steps in it), the scrape config (`prometheus.yml`), and a Grafana dashboard for the bot and the worker (`grafana/dashboards/hcbot.json`).

## Development and tests

```bash
make check
```

- **Unit and handler tests** (`go test ./...`) need no network. They use a fake Discord API that records every request.
- **Live tests** run against a separate test server:

  ```bash
  go test -tags live -run TestLive -count=1 -v ./internal/bot/
  ```

  They need `TEST_GUILD_ID` and `TEST_TARGET_TOKEN` (a second bot, used as the test member) in `.env`. For safety, they refuse to run on `GUILD_ID` or on a server with more than 5 members. They make `test-` roles, channels, emojis and stickers and delete them at the end.

---

## Data storage

`hcbot.db` (SQLite) in the working folder:

| Table | Content |
|---|---|
| `config` | Settings from `/setup` |
| `warnings` | Warnings |
| `user_notes` | Mod notes on members, with copies of the messages added from the right-click menu |
| `sentences` | Jail sentences |
| `tickets`, `ticket_notes` | Tickets and their notes |
| `ticket_archive` | Transcripts of tickets archived by an older version (read only) |
| `trigger_values` | Trigger responses |
| `autoreacts` | Autoreacts |
| `reaction_roles`, `reaction_role_pairs` | Reaction roles |
| `curated_messages` | Messages already curated |
| `perm_templates` | Permission templates |
| `scheduled_actions` | Tempban and MOTD timers |
| `pending_actions` | Data behind buttons, for example imports and wizard drafts |
| `markov_blocklist` | Words and phrases the Markov chat never learns or says |
| `tts_jobs` | `/say` clips that wait for the worker, with the text and the interaction token, until the clip is posted |
| `comic_characters` | The comic character that each member chose with `/character` |

`markov.db` (SQLite) in the working folder, or at `MARKOV_DB_PATH`:

| Table | Content |
|---|---|
| `words` | Each word the Markov chat knows |
| `transitions` | How often each pair of words came before each word |

---

## Credits

`/tweetshot` and the comics use these, and they are in the binary:

- [Liberation Sans](https://github.com/liberationfonts/liberation-fonts) 2.1.5, copyright Red Hat, Inc. and Google Corporation, under the SIL Open Font License 1.1 (`internal/comic/assets/LICENSE-LiberationSans.txt`), for the comics.

- [Noto Sans](https://github.com/notofonts/latin-greek-cyrillic) by the Noto Project Authors, under the SIL Open Font License 1.1 (`internal/tweetshot/assets/OFL-NotoSans.txt`).
- [Twemoji](https://github.com/jdecked/twemoji) emoji graphics, copyright 2019 Twitter, Inc and other contributors, under [CC-BY 4.0](https://creativecommons.org/licenses/by/4.0/) (`internal/richtext/assets/LICENSE-Twemoji-graphics.txt`). The graphics are not changed; the bot makes them smaller when it draws them.
