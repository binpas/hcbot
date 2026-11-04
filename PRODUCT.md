# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

- **Discord members of the HEALTH server** use the bot in Discord: slash commands, comics, reminders, tickets, and chat features. They never see the web page.
- **Mods and Administrators of the server** use the bot in Discord for moderation, and use the web page for management tasks. The operator is one of them and also runs the server. Other mods also use the page, so it must be clear without an explanation from the operator.

## Product Purpose

HEALTH Bot is the moderation, engagement, and community bot of the HEALTH Discord server. The web page is its control panel. It gives mods a better place than slash commands for tasks that are visual or large, such as managing comic art and giving characters to members. Success: a mod can complete a management task correctly, the first time, without help.

The control panel starts with comic art and member characters. It is planned to grow into the main place for bot administration, for example settings, reminders, triggers, and status. Comics are only the first area.

## Positioning

A bot and control panel made for one community: the HEALTH server, with the approval of the band. It is not a general bot for many servers, and it does not need to compete with one.

## Operating Context

- Discord is the main place of work. The web page supports it and does not replace the slash commands.
- The page is available only on the operator's local network, over HTTPS, with Discord login. For now, a mod must be on the operator's home network to use it.
- Mods use the page for short management tasks, with art files from their own computers.
- The worker site for text to speech is a separate operator tool, and it shows how the operator's tools are built now.

## Capabilities and Constraints

- **Now on the page:** add and remove custom characters and backgrounds, manage poses and face points, hide any art, make a character private, find a member, assign a character, lock a character, draw a test comic, and list art problems.
- **Access:** a member must have the mod role or Administrator on the server. Mods can do everything except destructive actions. Only Administrators can remove or replace art. Each change goes to the mod log with the member's name.
- **Removed files** go to a trash folder, so that a person can get them back. Built-in art cannot be changed, only hidden.
- **Technical:** one Go program that serves one HTML page, built into the program. The page must work with the current security rules, and the bot does not have the Server Members intent, so there is no full member list. Members are found by search or by user ID.
- **The repository is public.** Server IDs, private addresses, and secrets do not go in it.
- **Terms:** "persona", "jail", "rap sheet", "ticket", "trigger", and "autoreact" have fixed meanings in the bot (see README.md).
- **Undecided:** the order of later control panel areas.

## Brand Commitments

- The name is **HEALTH Bot**, for the HEALTH Discord server. The operator says that the server is official or approved by the band, so the bot may use the band's name as an official community tool. The bot can use the band's artwork, when the operator provides it.
- Documents are in ASD-STE100 Simplified Technical English, and page text follows the same plain style.

## Evidence on Hand

- `README.md` describes all the features and commands. `PRIVACY.md` and `TERMS.md` are the published policies.
- The comic art is not in the repository: Microsoft Comic Chat characters by Jim Woodring, from Microsoft's open-source release under the MIT license (`deploy/comic-art.sh`).
- The repository has no band logos, photos, or artwork yet. Do not make imitations of them, and do not write band statements, member counts, or testimonials that the operator did not give.

## Product Principles

1. **Discord first.** The page helps with tasks that are hard in Discord. A member or mod must not need the page for normal use.
2. **Safe by default.** Destructive actions need the correct role and a confirmation, and they can be undone where possible.
3. **Clear to any mod.** A mod who has never seen the page can complete a task with no help, and the page says what each change will do.
4. **Every change has a name on it.** Mods can see who changed what, in the mod log.
5. **Room to grow.** Each new area must fit in the same control panel, not become a separate tool.
