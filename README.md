# HEALTH Bot

A moderation, engagement and community bot for the HEALTH Discord server. It is written in Go with [discordgo](https://github.com/bwmarrin/discordgo) and a local SQLite database, and it is one static binary with no runtime dependencies.

---

## Features

- **Slash commands only.** Every command is a slash command. Triggers are the one exception: they fire on `@BotName <trigger>`.
- **Moderation:** ban, tempban, kick, mute/unmute, purge, and a self-timeout for members.
- **Warnings** with a rap sheet, single-warning removal, and automatic arrest and kick thresholds.
- **Jail:** restrict a member to one channel for a set time, with sentence tracking, automatic release, and `/jail audit` to find channels the jail role can still see.
- **Tickets:** members open private mod-support tickets with a reaction; mods close, archive, list, note and search them.
- **Triggers:** mod-defined keyword responses, with random picks from several responses and a Carl-bot import.
- **Autoreacts:** the bot reacts with an emote when a word or phrase is said. *(Needs the [Message Content option](#message-content-features).)*
- **Reaction roles** built with a wizard.
- **Markov chat:** the bot learns how people talk in chosen channels and replies with its own mix of their words. It has a blocklist and tools to remove words. Its data is in a separate database file.
- **Curation (starboard):** messages with enough reactions are forwarded to a curated channel. *(Needs the [Message Content option](#message-content-features).)*
- **Audit logging:** messages, channels, threads, roles, voice, bans and invites.
- **Admin tools:** database backups (also daily), emoji/sticker export and import, a channel permissions report, permission templates, and channel archiving.
- **In-Discord configuration** with `/setup`. You do not need to edit `.env` for most settings.
- **Timers survive a restart:** tempban unbans, Member Of The Day removal, and jail releases are stored in the database.

---

## Requirements

- **Go 1.27 or later** to build. The server that runs the bot needs no Go and no C libraries: the SQLite driver is pure Go.
- A **Discord application with a bot user**, from the [Discord Developer Portal](https://discord.com/developers/applications).
- The bot invited to your server with the permissions in [Required bot permissions](#required-bot-permissions).

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

Keep `.env` private: the token gives full control of the bot. If a token is ever shown in a log or a chat, reset it in the Developer Portal.

### `/setup`

Run `/setup` as a server administrator. It shows an ephemeral embed (only you see it) with a few settings on each page. Channel, category and role settings use a dropdown. Number, emote and text settings use an **Edit** button. Each change is saved at once. The session expires after 5 minutes without use.

| Setting | What it is | Default |
|---|---|---|
| `MOD_LOG` | Channel for the mod log | — |
| `BIG_BROTHER` | Channel for the audit log | — |
| `SENSITIVE_LOG` | Channel for sensitive output (`/userinfo`). Falls back to `BIG_BROTHER` | — |
| `CURATED` | Channel for curated messages ¹ | — |
| `TICKET_CAT` | **Category** for new tickets | — |
| `MOD_SUPPORT` | Channel for the mod support embed | — |
| `MOD_ROLE` | The moderator role | — |
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
| `MARKOV_REPLY_CHANCE` | Percent chance (0–100) that the Markov chat talks after a normal message in a reply channel | 0 |
| `MARKOV_RANDOM_AS_REPLY` | Percent (0–100) of those random messages sent as a reply to the message. The rest are posted in the channel without a reply. | 50 |
| `MARKOV_COOLDOWN` | Seconds after a Markov message before the chat talks again in the same channel | 60 |
| `MARKOV_ALWAYS_REPLY_MENTION` | **On:** always reply to an @mention, also during the cooldown. **Off:** a mention gets a reply only when the cooldown is over; mods always get a reply. (A button in `/setup`.) | on |
| `MARKOV_IMPORT_MAX_MB` | Largest file for `/markov import`, in MB | 25 |

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
- **media recovery** for deleted messages.

When the option is off, the `/autoreact` commands are not registered, curation is off completely, and `/setup` and `/help` do not show them. The data stays in the database, so everything works again when you turn the option on.

The bot then also requests the Message Content intent, so you must also turn on **Message Content Intent** under **Bot** in the Developer Portal. Restart the bot after a change.

Two important facts, from tests with the real Discord API:

1. **Discord decides access to message text from the portal switch**, not from the intent that the code requests. When the switch is on, Discord sends the text of every message to the bot, also when `ENABLE_MESSAGE_CONTENT_FEATURES` is off. The bot stores and shows message text only when the option is on.
2. **A forward needs access to the message text.** Discord refuses to forward a message whose text the bot cannot read (error 160014), except a message that mentions the bot. This is why curation needs the option: it forwards each curated message.

---

## Required bot permissions

In **OAuth2 → URL Generator**, select the `bot` and `applications.commands` scopes, and give at least these permissions:

- View Channels, Send Messages, Read Message History, Add Reactions, Embed Links, Attach Files
- Manage Messages *(purge, reaction clean-up)*
- Manage Roles *(jail, MOTD, reaction roles, permission overwrites)*
- Manage Channels *(tickets, archives, `/channelarchive`, `/jail setup`)*
- Manage Server *(invite logging and ❌ invite deletion)*
- Manage Expressions and Create Expressions *(`/assets`)*
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
| `/trigger add <name> <value>` | mod only | Makes a trigger, or replaces its responses with one value. |
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
| `/markov status` | mod only | The size of the chain, its channels and the mode. |
| `/markov forget <text>` | mod only | Removes a word, or a phrase, from the chain, after you confirm. |
| `/markov blockadd <text>` | mod only | Blocks a word or phrase. Then offers to remove it from the chain. |
| `/markov blockremove <text>` | mod only | Removes a word or phrase from the blocklist. |
| `/markov blocklist` | mod only | All blocked words and phrases (hidden as spoilers). |
| `/markov import <file>` | mod only | Trains the chain from a `.txt` file (one message on each line) or a DiscordChatExporter `.json` export. |
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

- **Curation** (only with `ENABLE_MESSAGE_CONTENT_FEATURES`): when a message gets `CURATED_THRESHOLD` × `CURATED_EMOTE`, the bot posts an info card (author, count, channel) in `CURATED`, then forwards the message. If the forward fails, the bot tries once more after 2 seconds, then posts a link to the message. Each message is curated only once.
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
- **Random talk:** after a normal message, the bot talks with a chance of `MARKOV_REPLY_CHANCE` percent. `MARKOV_RANDOM_AS_REPLY` percent of these messages are replies to the message; the others are posted in the channel. This also works without Message Content, but then the bot cannot read the message, so it starts at a random place in the chain.
- **Cooldown:** after the bot talks in a channel, it waits `MARKOV_COOLDOWN` seconds before random talk in that channel again. Each channel has its own timer, and a restart resets the timers. With `MARKOV_ALWAYS_REPLY_MENTION` on, mentions always get a reply; with it off, mentions also wait for the cooldown. Mentions from mods (`MOD_ROLE`, the Administrator permission, or the server owner) always get a reply. Each Markov message starts the cooldown, also a reply to a mention.

A trigger wins: `@BotName <trigger>` posts the trigger, not a Markov reply.

**What it learns:** the words of each message, in lowercase, with the pairs of words that come before each word. It does not keep who said a message, or the full message. It leaves out mentions, `@everyone`, `@here`, links, and words longer than 64 characters. It ignores a full message that contains a blocked word or phrase. Replies cannot ping anyone.

**Blocklist:** a blocked entry matches whole words, without case and without punctuation at either end. For example, `cat` blocks `Cat!` but not `cats`. A phrase blocks those words in that sequence. The blocklist is in `hcbot.db`, so it stays when you delete the chain.

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

`markov.db` (SQLite) in the working folder, or at `MARKOV_DB_PATH`:

| Table | Content |
|---|---|
| `words` | Each word the Markov chat knows |
| `transitions` | How often each pair of words came before each word |
