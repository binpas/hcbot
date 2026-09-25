package bot

import "github.com/bwmarrin/discordgo"

// Colours matching discord.py's Colour presets.
const (
	colourBlurple = 0x5865F2
	colourGreen   = 0x2ECC71
	colourRed     = 0xE74C3C
)

func makeEmbed(title, description string, colour int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Title: title, Description: description, Color: colour}
}

func okEmbed(description string) *discordgo.MessageEmbed {
	return makeEmbed("✅", description, colourGreen)
}

func errEmbed(description string) *discordgo.MessageEmbed {
	return makeEmbed("❌ Error", description, colourRed)
}

func channelMention(id string) string { return "<#" + id + ">" }
func roleMention(id string) string    { return "<@&" + id + ">" }
