package moderation

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"emperror.dev/errors"
	"github.com/botlabs-gg/yagpdb/analytics"
	"github.com/botlabs-gg/yagpdb/bot"
	"github.com/botlabs-gg/yagpdb/bot/paginatedmessages"
	"github.com/botlabs-gg/yagpdb/commands"
	"github.com/botlabs-gg/yagpdb/common"
	"github.com/botlabs-gg/yagpdb/common/scheduledevents2"
	"github.com/jinzhu/gorm"
	"github.com/jonas747/dcmd/v4"
	"github.com/jonas747/discordgo/v2"
	"github.com/jonas747/dstate/v4"
)

func MBaseCmd(cmdData *dcmd.Data, targetID int64) (config *Config, targetUser *discordgo.User, err error) {
	config, err = GetConfig(cmdData.GuildData.GS.ID)
	if err != nil {
		return nil, nil, errors.WithMessage(err, "GetConfig")
	}

	if targetID != 0 {
		targetMember, _ := bot.GetMember(cmdData.GuildData.GS.ID, targetID)
		if targetMember != nil {
			gs := cmdData.GuildData.GS

			above := bot.IsMemberAbove(gs, cmdData.GuildData.MS, targetMember)

			if !above {
				return config, &targetMember.User, commands.NewUserError("Can't use moderation commands on users ranked the same or higher than you")
			}

			return config, &targetMember.User, nil
		}
	}

	return config, &discordgo.User{
		Username:      "unknown",
		Discriminator: "????",
		ID:            targetID,
	}, nil

}

func MBaseCmdSecond(cmdData *dcmd.Data, reason string, reasonArgOptional bool, neededPerm int64, additionalPermRoles []int64, enabled bool) (oreason string, err error) {
	cmdName := cmdData.Cmd.Trigger.Names[0]
	oreason = reason
	if !enabled {
		return oreason, commands.NewUserErrorf("The **%s** command is disabled on this server. Enable it in the control panel on the moderation page.", cmdName)
	}

	if strings.TrimSpace(reason) == "" {
		if !reasonArgOptional {
			return oreason, commands.NewUserError("A reason has been set to be required for this command by the server admins, see help for more info.")
		}

		oreason = "(No reason specified)"
	}

	member := cmdData.GuildData.MS

	// check permissions or role setup for this command
	permsMet := false
	if len(additionalPermRoles) > 0 {
		// Check if the user has one of the required roles
		for _, r := range member.Member.Roles {
			if common.ContainsInt64Slice(additionalPermRoles, r) {
				permsMet = true
				break
			}
		}
	}

	if !permsMet && neededPerm != 0 {
		// Fallback to legacy permissions
		hasPerms, err := bot.AdminOrPermMS(cmdData.GuildData.GS.ID, cmdData.ChannelID, member, neededPerm)
		if err != nil || !hasPerms {
			return oreason, commands.NewUserErrorf("The **%s** command requires the **%s** permission in this channel or additional roles set up by admins, you don't have it. (if you do contact bot support)", cmdName, common.StringPerms[neededPerm])
		}

		permsMet = true
	}

	go analytics.RecordActiveUnit(cmdData.GuildData.GS.ID, &Plugin{}, "executed_cmd_"+cmdName)

	return oreason, nil
}

func SafeArgString(data *dcmd.Data, arg int) string {
	if arg >= len(data.Args) || data.Args[arg].Value == nil {
		return ""
	}

	return data.Args[arg].Str()
}

func GenericCmdResp(action ModlogAction, target *discordgo.User, duration time.Duration, zeroDurPermanent bool, noDur bool) string {
	durStr := " indefinitely"
	if duration > 0 || !zeroDurPermanent {
		durStr = " for `" + common.HumanizeDuration(common.DurationPrecisionMinutes, duration) + "`"
	}
	if noDur {
		durStr = ""
	}

	userStr := target.Username + "#" + target.Discriminator
	if target.Discriminator == "????" {
		userStr = strconv.FormatInt(target.ID, 10)
	}

	return fmt.Sprintf("%s %s `%s`%s", action.Emoji, action.Prefix, userStr, durStr)
}

var ModerationCommands = []*commands.YAGCommand{
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Ban",
		Aliases:       []string{"banid"},
		Description:   "Bans a member, specify a duration with -d and specify number of days of messages to delete with -ddays (0 to 7)",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Reason", Type: dcmd.String},
		},
		ArgSwitches: []*dcmd.ArgDef{
			{Name: "d", Help: "Duration", Type: &commands.DurationArg{}, Default: time.Duration(0)},
			{Name: "ddays", Help: "Delete Days", Type: dcmd.Int},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionBanMembers}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			reason := SafeArgString(parsed, 1)
			reason, err = MBaseCmdSecond(parsed, reason, config.BanReasonOptional, discordgo.PermissionBanMembers, config.BanCmdRoles, config.BanEnabled)
			if err != nil {
				return nil, err
			}

			if utf8.RuneCountInString(reason) > 470 {
				return "Error: Reason too long (can be max 470 characters).", nil
			}

			ddays := int(config.DefaultBanDeleteDays.Int64)
			if parsed.Switches["ddays"].Value != nil {
				ddays = parsed.Switches["ddays"].Int()
			}
			var msg *discordgo.Message
			if parsed.TraditionalTriggerData != nil {
				msg = parsed.TraditionalTriggerData.Message
			}
			err = BanUserWithDuration(config, parsed.GuildData.GS.ID, parsed.GuildData.CS, msg, parsed.Author, reason, target, parsed.Switches["d"].Value.(time.Duration), ddays)
			if err != nil {
				return nil, err
			}

			return GenericCmdResp(MABanned, target, parsed.Switch("d").Value.(time.Duration), true, false), nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Unban",
		Aliases:       []string{"unbanid"},
		Description:   "Unbans a user. Reason requirement is same as ban command setting.",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Reason", Type: dcmd.String},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionBanMembers}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, _, err := MBaseCmd(parsed, 0) //No need to check member role hierarchy as banned members should not be in server
			if err != nil {
				return nil, err
			}

			reason := SafeArgString(parsed, 1)
			reason, err = MBaseCmdSecond(parsed, reason, config.BanReasonOptional, discordgo.PermissionBanMembers, config.BanCmdRoles, config.BanEnabled)
			if err != nil {
				return nil, err
			}
			targetID := parsed.Args[0].Int64()
			target := &discordgo.User{
				Username:      "unknown",
				Discriminator: "????",
				ID:            targetID,
			}
			targetMem, _ := bot.GetMember(parsed.GuildData.GS.ID, targetID)
			if targetMem != nil {
				return "User is not banned!", nil
			}

			isNotBanned, err := UnbanUser(config, parsed.GuildData.GS.ID, parsed.Author, reason, target)

			if err != nil {
				return nil, err
			}
			if isNotBanned {
				return "User is not banned!", nil
			}

			return GenericCmdResp(MAUnbanned, target, 0, true, true), nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Kick",
		Description:   "Kicks a member",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Reason", Type: dcmd.String},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionKickMembers}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			reason := SafeArgString(parsed, 1)
			reason, err = MBaseCmdSecond(parsed, reason, config.KickReasonOptional, discordgo.PermissionKickMembers, config.KickCmdRoles, config.KickEnabled)
			if err != nil {
				return nil, err
			}

			if utf8.RuneCountInString(reason) > 470 {
				return "Error: Reason too long (can be max 470 characters).", nil
			}

			var msg *discordgo.Message
			if parsed.TraditionalTriggerData != nil {
				msg = parsed.TraditionalTriggerData.Message
			}
			err = KickUser(config, parsed.GuildData.GS.ID, parsed.GuildData.CS, msg, parsed.Author, reason, target)
			if err != nil {
				return nil, err
			}

			return GenericCmdResp(MAKick, target, 0, true, true), nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Mute",
		Description:   "Mutes a member",
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Duration", Type: &commands.DurationArg{}},
			{Name: "Reason", Type: dcmd.String},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionManageRoles}},
		ArgumentCombos:      [][]int{{0, 1, 2}, {0, 2, 1}, {0, 1}, {0, 2}, {0}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			if config.MuteRole == "" {
				return "No mute role set up, assign a mute role in the control panel", nil
			}

			reason := parsed.Args[2].Str()
			reason, err = MBaseCmdSecond(parsed, reason, config.MuteReasonOptional, discordgo.PermissionKickMembers, config.MuteCmdRoles, config.MuteEnabled)
			if err != nil {
				return nil, err
			}

			d := time.Duration(config.DefaultMuteDuration.Int64) * time.Minute
			if parsed.Args[1].Value != nil {
				d = parsed.Args[1].Value.(time.Duration)
			}
			if d > 0 && d < time.Minute {
				d = time.Minute
			}

			logger.Info(d.Seconds())

			member, err := bot.GetMember(parsed.GuildData.GS.ID, target.ID)
			if err != nil || member == nil {
				return "Member not found", err
			}

			var msg *discordgo.Message
			if parsed.TraditionalTriggerData != nil {
				msg = parsed.TraditionalTriggerData.Message
			}
			err = MuteUnmuteUser(config, true, parsed.GuildData.GS.ID, parsed.GuildData.CS, msg, parsed.Author, reason, member, int(d.Minutes()))
			if err != nil {
				return nil, err
			}

			return GenericCmdResp(MAMute, target, d, true, false), nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Unmute",
		Description:   "Unmutes a member",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Reason", Type: dcmd.String},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionManageRoles}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			if config.MuteRole == "" {
				return "No mute role set up, assign a mute role in the control panel", nil
			}

			reason := parsed.Args[1].Str()
			reason, err = MBaseCmdSecond(parsed, reason, config.UnmuteReasonOptional, discordgo.PermissionKickMembers, config.MuteCmdRoles, config.MuteEnabled)
			if err != nil {
				return nil, err
			}

			member, err := bot.GetMember(parsed.GuildData.GS.ID, target.ID)
			if err != nil || member == nil {
				return "Member not found", err
			}

			var msg *discordgo.Message
			if parsed.TraditionalTriggerData != nil {
				msg = parsed.TraditionalTriggerData.Message
			}
			err = MuteUnmuteUser(config, false, parsed.GuildData.GS.ID, parsed.GuildData.CS, msg, parsed.Author, reason, member, 0)
			if err != nil {
				return nil, err
			}

			return GenericCmdResp(MAUnmute, target, 0, false, true), nil
		},
	},
	{
		CustomEnabled: true,
		Cooldown:      5,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Report",
		Description:   "Reports a member to the server's staff",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Reason", Type: dcmd.String},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, 0, nil, config.ReportEnabled)
			if err != nil {
				return nil, err
			}

			temp, err := bot.GetMember(parsed.GuildData.GS.ID, parsed.Args[0].Int64())
			if err != nil || temp == nil {
				return nil, err
			}

			target := temp.User

			if target.ID == parsed.Author.ID {
				return "You can't report yourself, silly.", nil
			}

			logLink := CreateLogs(parsed.GuildData.GS.ID, parsed.GuildData.CS.ID, parsed.Author)

			channelID := config.IntReportChannel()
			if channelID == 0 {
				return "No report channel set up", nil
			}

			topContent := fmt.Sprintf("%s reported %s", parsed.Author.Mention(), target.Mention())

			embed := &discordgo.MessageEmbed{
				Author: &discordgo.MessageEmbedAuthor{
					Name:    fmt.Sprintf("%s#%s (ID %d)", parsed.Author.Username, parsed.Author.Discriminator, parsed.Author.ID),
					IconURL: discordgo.EndpointUserAvatar(parsed.Author.ID, parsed.Author.Avatar),
				},
				Description: fmt.Sprintf("🔍**Reported** %s#%s *(ID %d)*\n📄**Reason:** %s ([Logs](%s))\n**Channel:** <#%d>", target.Username, target.Discriminator, target.ID, parsed.Args[1].Value, logLink, parsed.ChannelID),
				Color:       0xee82ee,
				Thumbnail: &discordgo.MessageEmbedThumbnail{
					URL: discordgo.EndpointUserAvatar(target.ID, target.Avatar),
				},
			}

			send := &discordgo.MessageSend{
				Content: topContent,
				Embed:   embed,
				AllowedMentions: discordgo.AllowedMentions{
					Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers},
				},
			}

			_, err = common.BotSession.ChannelMessageSendComplex(channelID, send)
			if err != nil {
				return "Something went wrong while sending your report!", err
			}

			// Don't bother sending confirmation if it is done in the report channel
			if channelID != parsed.ChannelID {
				return "User reported to the proper authorities!", nil
			}

			return nil, nil
		},
	},
	{
		CustomEnabled:   true,
		CmdCategory:     commands.CategoryModeration,
		Name:            "Clean",
		Description:     "Delete the last number of messages from chat, optionally filtering by user, max age and regex or ignoring pinned messages.\n⚠️ **Warning:** Using `clean <userId> <amount>` does not work. This is because the user ID is interpreted as the amount. As it is over the limit of 100, it is treated as invalid. You can use `clean <amount> <userId>` instead or mention the user.",
		LongDescription: "Specify a regex with \"-r regex_here\" and max age with \"-ma 1h10m\"\nYou can invert the regex match (i.e. only clear messages that do not match the given regex) by supplying the `-im` flag\nNote: Will only look in the last 1k messages",
		Aliases:         []string{"clear", "cl"},
		RequiredArgs:    1,
		Arguments: []*dcmd.ArgDef{
			{Name: "Num", Type: &dcmd.IntArg{Min: 1, Max: 100}},
			{Name: "User", Type: dcmd.UserID, Default: 0},
		},
		ArgSwitches: []*dcmd.ArgDef{
			{Name: "r", Help: "Regex", Type: dcmd.String},
			{Name: "im", Help: "Invert regex match"},
			{Name: "ma", Help: "Max age", Default: time.Duration(0), Type: &commands.DurationArg{}},
			{Name: "minage", Help: "Min age", Default: time.Duration(0), Type: &commands.DurationArg{}},
			{Name: "i", Help: "Regex case insensitive"},
			{Name: "nopin", Help: "Ignore pinned messages"},
			{Name: "a", Help: "Only remove messages with attachments"},
			{Name: "to", Help: "Stop at this msg ID", Type: dcmd.BigInt},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionManageMessages}},
		ArgumentCombos:      [][]int{{0}, {0, 1}, {1, 0}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			botMember, err := bot.GetMember(parsed.GuildData.GS.ID, common.BotUser.ID)
			if err != nil {
				return "Failed fetching bot member to check permissions", nil
			}

			canClear, err := bot.AdminOrPermMS(parsed.GuildData.GS.ID, parsed.ChannelID, botMember, discordgo.PermissionManageMessages)
			if err != nil || !canClear {
				return "I need the `Manage Messages` permission to be able to clear messages", nil
			}

			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, nil, config.CleanEnabled)
			if err != nil {
				return nil, err
			}

			userFilter := parsed.Args[1].Int64()

			num := parsed.Args[0].Int()
			if (userFilter == 0 || userFilter == parsed.Author.ID) && parsed.Source != 0 {
				num++ // Automatically include our own message if not triggeded by exec/execAdmin
			}

			if num > 100 {
				num = 100
			}

			if num < 1 {
				if num < 0 {
					return errors.New("Bot is having a stroke <https://www.youtube.com/watch?v=dQw4w9WgXcQ>"), nil
				}
				return errors.New("Can't delete nothing"), nil
			}

			filtered := false

			// Check if we should regex match this
			re := ""
			if parsed.Switches["r"].Value != nil {
				filtered = true
				re = parsed.Switches["r"].Str()

				// Add the case insensitive flag if needed
				if parsed.Switches["i"].Value != nil && parsed.Switches["i"].Value.(bool) {
					if !strings.HasPrefix(re, "(?i)") {
						re = "(?i)" + re
					}
				}
			}
			invertRegexMatch := parsed.Switch("im").Value != nil && parsed.Switch("im").Value.(bool)

			// Check if we have a max age
			ma := parsed.Switches["ma"].Value.(time.Duration)
			if ma != 0 {
				filtered = true
			}

			// Check if we have a min age
			minAge := parsed.Switches["minage"].Value.(time.Duration)
			if minAge != 0 {
				filtered = true
			}

			// Check if set to break at a certain ID
			toID := int64(0)
			if parsed.Switches["to"].Value != nil {
				filtered = true
				toID = parsed.Switches["to"].Int64()
			}

			// Check if we should ignore pinned messages
			pe := false
			if parsed.Switches["nopin"].Value != nil && parsed.Switches["nopin"].Value.(bool) {
				pe = true
				filtered = true
			}

			// Check if we should only delete messages with attachments
			attachments := false
			if parsed.Switches["a"].Value != nil && parsed.Switches["a"].Value.(bool) {
				attachments = true
				filtered = true
			}

			limitFetch := num
			if userFilter != 0 || filtered {
				limitFetch = num * 50 // Maybe just change to full fetch?
			}

			if limitFetch > 1000 {
				limitFetch = 1000
			}

			// Wait a second so the client dosen't gltich out
			time.Sleep(time.Second)

			numDeleted, err := AdvancedDeleteMessages(parsed.GuildData.GS.ID, parsed.ChannelID, userFilter, re, invertRegexMatch, toID, ma, minAge, pe, attachments, num, limitFetch)
			return dcmd.NewTemporaryResponse(time.Second*5, fmt.Sprintf("Deleted %d message(s)! :')", numDeleted), true), err
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Reason",
		Description:   "Add/Edit a modlog reason",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			{Name: "Message-ID", Type: dcmd.BigInt},
			{Name: "Reason", Type: dcmd.String},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionKickMembers, nil, true)
			if err != nil {
				return nil, err
			}

			if config.ActionChannel == "" {
				return "No mod log channel set up", nil
			}

			msg, err := common.BotSession.ChannelMessage(config.IntActionChannel(), parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			if msg.Author.ID != common.BotUser.ID {
				return "I didn't make that message", nil
			}

			if len(msg.Embeds) < 1 {
				return "This entry is either too old or you're trying to mess with me...", nil
			}

			embed := msg.Embeds[0]
			updateEmbedReason(parsed.Author, parsed.Args[1].Str(), embed)
			_, err = common.BotSession.ChannelMessageEditEmbed(config.IntActionChannel(), msg.ID, embed)
			if err != nil {
				return nil, err
			}

			return "👌", nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Warn",
		Description:   "Warns a user, warnings are saved using the bot. Use -warnings to view them.",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Reason", Type: dcmd.String},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}
			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, config.WarnCmdRoles, config.WarnCommandsEnabled)
			if err != nil {
				return nil, err
			}

			var msg *discordgo.Message
			if parsed.TraditionalTriggerData != nil {
				msg = parsed.TraditionalTriggerData.Message
			}
			err = WarnUser(config, parsed.GuildData.GS.ID, parsed.GuildData.CS, msg, parsed.Author, target, parsed.Args[1].Str())
			if err != nil {
				return nil, err
			}

			return GenericCmdResp(MAWarned, target, 0, false, true), nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "Warnings",
		Description:   "Lists warning of a user.",
		Aliases:       []string{"Warns"},
		RequiredArgs:  0,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID, Default: 0},
			{Name: "Page", Type: &dcmd.IntArg{Max: 10000}, Default: 0},
		},
		ArgSwitches: []*dcmd.ArgDef{
			{Name: "id", Help: "Warning ID", Type: dcmd.Int},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			var err error
			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, config.WarnCmdRoles, true)
			if err != nil {
				return nil, err
			}

			if parsed.Switches["id"].Value != nil {
				var warn []*WarningModel
				err = common.GORM.Where("guild_id = ? AND id = ?", parsed.GuildData.GS.ID, parsed.Switches["id"].Int()).First(&warn).Error
				if err != nil && err != gorm.ErrRecordNotFound {
					return nil, err
				}
				if len(warn) == 0 {
					return fmt.Sprintf("Warning with given id : `%d` does not exist.", parsed.Switches["id"].Int()), nil
				}

				return &discordgo.MessageEmbed{
					Title:       fmt.Sprintf("Warning#%d - User : %s", warn[0].ID, warn[0].UserID),
					Description: fmt.Sprintf("`%20s` - **Reason** : %s", warn[0].CreatedAt.UTC().Format(time.RFC822), warn[0].Message),
					Footer:      &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("By: %s (%13s)", warn[0].AuthorUsernameDiscrim, warn[0].AuthorID)},
				}, nil
			}
			page := parsed.Args[1].Int()
			if page < 1 {
				page = 1
			}
			if parsed.Context().Value(paginatedmessages.CtxKeyNoPagination) != nil {
				return PaginateWarnings(parsed)(nil, page)
			}
			_, err = paginatedmessages.CreatePaginatedMessage(parsed.GuildData.GS.ID, parsed.GuildData.CS.ID, page, 0, PaginateWarnings(parsed))
			return nil, err
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "EditWarning",
		Description:   "Edit a warning, id is the first number of each warning from the warnings command",
		RequiredArgs:  2,
		Arguments: []*dcmd.ArgDef{
			{Name: "Id", Type: dcmd.Int},
			{Name: "NewMessage", Type: dcmd.String},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, config.WarnCmdRoles, config.WarnCommandsEnabled)
			if err != nil {
				return nil, err
			}

			rows := common.GORM.Model(WarningModel{}).Where("guild_id = ? AND id = ?", parsed.GuildData.GS.ID, parsed.Args[0].Int()).Update(
				"message", fmt.Sprintf("%s (updated by %s#%s (%d))", parsed.Args[1].Str(), parsed.Author.Username, parsed.Author.Discriminator, parsed.Author.ID)).RowsAffected

			if rows < 1 {
				return "Failed updating, most likely couldn't find the warning", nil
			}

			return "👌", nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "DelWarning",
		Aliases:       []string{"dw"},
		Description:   "Deletes a warning, id is the first number of each warning from the warnings command",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			{Name: "Id", Type: dcmd.Int},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, config.WarnCmdRoles, config.WarnCommandsEnabled)
			if err != nil {
				return nil, err
			}

			rows := common.GORM.Where("guild_id = ? AND id = ?", parsed.GuildData.GS.ID, parsed.Args[0].Int()).Delete(WarningModel{}).RowsAffected
			if rows < 1 {
				return "Failed deleting, most likely couldn't find the warning", nil
			}

			return "👌", nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "ClearWarnings",
		Aliases:       []string{"clw"},
		Description:   "Clears the warnings of a user",
		RequiredArgs:  1,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {

			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, config.WarnCmdRoles, config.WarnCommandsEnabled)
			if err != nil {
				return nil, err
			}

			userID := parsed.Args[0].Int64()

			rows := common.GORM.Where("guild_id = ? AND user_id = ?", parsed.GuildData.GS.ID, userID).Delete(WarningModel{}).RowsAffected
			return fmt.Sprintf("Deleted %d warnings.", rows), nil
		},
	},
	{
		CmdCategory: commands.CategoryModeration,
		Name:        "TopWarnings",
		Aliases:     []string{"topwarns"},
		Description: "Shows ranked list of warnings on the server",
		Arguments: []*dcmd.ArgDef{
			{Name: "Page", Type: dcmd.Int, Default: 0},
		},
		ArgSwitches: []*dcmd.ArgDef{
			{Name: "id", Help: "List userIDs"},
		},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: paginatedmessages.PaginatedCommand(0, func(parsed *dcmd.Data, p *paginatedmessages.PaginatedMessage, page int) (*discordgo.MessageEmbed, error) {

			showUserIDs := false
			config, _, err := MBaseCmd(parsed, 0)
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageMessages, config.WarnCmdRoles, true)
			if err != nil {
				return nil, err
			}

			if parsed.Switches["id"].Value != nil && parsed.Switches["id"].Value.(bool) {
				showUserIDs = true
			}

			offset := (page - 1) * 15
			entries, err := TopWarns(parsed.GuildData.GS.ID, offset, 15)
			if err != nil {
				return nil, err
			}

			if len(entries) < 1 && p != nil && p.LastResponse != nil { //Don't send No Results error on first execution.
				return nil, paginatedmessages.ErrNoResults
			}

			embed := &discordgo.MessageEmbed{
				Title: "Ranked list of warnings",
			}

			out := "```\n# - Warns - User\n"
			for _, v := range entries {
				if !showUserIDs {
					user := v.Username
					if user == "" {
						user = "unknown ID:" + strconv.FormatInt(v.UserID, 10)
					}
					out += fmt.Sprintf("#%02d: %4d - %s\n", v.Rank, v.WarnCount, user)
				} else {
					out += fmt.Sprintf("#%02d: %4d - %d\n", v.Rank, v.WarnCount, v.UserID)
				}
			}
			out += "```\n"

			embed.Description = out

			return embed, nil

		}),
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "GiveRole",
		Aliases:       []string{"grole", "arole", "addrole"},
		Description:   "Gives a role to the specified member, with optional expiry",

		RequiredArgs: 2,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Role", Type: dcmd.String},
		},
		ArgSwitches: []*dcmd.ArgDef{
			{Name: "d", Default: time.Duration(0), Help: "Duration", Type: &commands.DurationArg{}},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionManageRoles}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageRoles, config.GiveRoleCmdRoles, config.GiveRoleCmdEnabled)
			if err != nil {
				return nil, err
			}

			member, err := bot.GetMember(parsed.GuildData.GS.ID, target.ID)
			if err != nil || member == nil {
				return "Member not found", err
			}

			role := FindRole(parsed.GuildData.GS, parsed.Args[1].Str())
			if role == nil {
				return "Couldn't find the specified role", nil
			}

			if !bot.IsMemberAboveRole(parsed.GuildData.GS, parsed.GuildData.MS, role) {
				return "Can't give roles above you", nil
			}

			dur := parsed.Switches["d"].Value.(time.Duration)

			// no point if the user has the role and is not updating the expiracy
			if common.ContainsInt64Slice(member.Member.Roles, role.ID) && dur <= 0 {
				return "That user already has that role", nil
			}

			err = common.AddRoleDS(member, role.ID)
			if err != nil {
				return nil, err
			}

			// schedule the expiry
			if dur > 0 {
				err := scheduledevents2.ScheduleRemoveRole(parsed.Context(), parsed.GuildData.GS.ID, target.ID, role.ID, time.Now().Add(dur))
				if err != nil {
					return nil, err
				}
			}

			// cancel the event to add the role
			scheduledevents2.CancelAddRole(parsed.Context(), parsed.GuildData.GS.ID, parsed.Author.ID, role.ID)

			action := MAGiveRole
			action.Prefix = "Gave the role " + role.Name + " to "
			if config.GiveRoleCmdModlog && config.IntActionChannel() != 0 {
				if dur > 0 {
					action.Footer = "Duration: " + common.HumanizeDuration(common.DurationPrecisionMinutes, dur)
				}
				CreateModlogEmbed(config, parsed.Author, action, target, "", "")
			}

			return GenericCmdResp(action, target, dur, true, dur <= 0), nil
		},
	},
	{
		CustomEnabled: true,
		CmdCategory:   commands.CategoryModeration,
		Name:          "RemoveRole",
		Aliases:       []string{"rrole", "takerole", "trole"},
		Description:   "Removes the specified role from the target",

		RequiredArgs: 2,
		Arguments: []*dcmd.ArgDef{
			{Name: "User", Type: dcmd.UserID},
			{Name: "Role", Type: dcmd.String},
		},
		RequireBotPerms:     [][]int64{{discordgo.PermissionAdministrator}, {discordgo.PermissionManageServer}, {discordgo.PermissionManageRoles}},
		SlashCommandEnabled: true,
		DefaultEnabled:      false,
		RunFunc: func(parsed *dcmd.Data) (interface{}, error) {
			config, target, err := MBaseCmd(parsed, parsed.Args[0].Int64())
			if err != nil {
				return nil, err
			}

			_, err = MBaseCmdSecond(parsed, "", true, discordgo.PermissionManageRoles, config.GiveRoleCmdRoles, config.GiveRoleCmdEnabled)
			if err != nil {
				return nil, err
			}

			member, err := bot.GetMember(parsed.GuildData.GS.ID, target.ID)
			if err != nil || member == nil {
				return "Member not found", err
			}

			role := FindRole(parsed.GuildData.GS, parsed.Args[1].Str())
			if role == nil {
				return "Couldn't find the specified role", nil
			}

			if !bot.IsMemberAboveRole(parsed.GuildData.GS, parsed.GuildData.MS, role) {
				return "Can't remove roles above you", nil
			}

			err = common.RemoveRoleDS(member, role.ID)
			if err != nil {
				return nil, err
			}

			// cancel the event to remove the role
			scheduledevents2.CancelRemoveRole(parsed.Context(), parsed.GuildData.GS.ID, parsed.Author.ID, role.ID)

			action := MARemoveRole
			action.Prefix = "Removed the role " + role.Name + " from "
			if config.GiveRoleCmdModlog && config.IntActionChannel() != 0 {
				CreateModlogEmbed(config, parsed.Author, action, target, "", "")
			}

			return GenericCmdResp(action, target, 0, true, true), nil
		},
	},
}

func AdvancedDeleteMessages(guildID, channelID int64, filterUser int64, regex string, invertRegexMatch bool, toID int64, maxAge time.Duration, minAge time.Duration, pinFilterEnable bool, attachmentFilterEnable bool, deleteNum, fetchNum int) (int, error) {
	var compiledRegex *regexp.Regexp
	if regex != "" {
		// Start by compiling the regex
		var err error
		compiledRegex, err = regexp.Compile(regex)
		if err != nil {
			return 0, err
		}
	}

	var pinnedMessages map[int64]struct{}
	if pinFilterEnable {
		//Fetch pinned messages from channel and make a map with ids as keys which will make it easy to verify if a message with a given ID is pinned message
		messageSlice, err := common.BotSession.ChannelMessagesPinned(channelID)
		if err != nil {
			return 0, err
		}
		pinnedMessages = make(map[int64]struct{}, len(messageSlice))
		for _, msg := range messageSlice {
			pinnedMessages[msg.ID] = struct{}{} //empty struct works because we are not really interested in value
		}
	}

	msgs, err := bot.GetMessages(guildID, channelID, fetchNum, false)
	if err != nil {
		return 0, err
	}

	toDelete := make([]int64, 0)
	now := time.Now()
	for i := 0; i < len(msgs); i++ {
		if filterUser != 0 && msgs[i].Author.ID != filterUser {
			continue
		}

		// Can only bulk delete messages up to 2 weeks (but add 1 minute buffer account for time sync issues and other smallies)
		if now.Sub(msgs[i].ParsedCreatedAt) > (time.Hour*24*14)-time.Minute {
			continue
		}

		// Check regex
		if compiledRegex != nil {
			ok := compiledRegex.MatchString(msgs[i].Content)
			if invertRegexMatch {
				ok = !ok
			}
			if !ok {
				continue
			}
		}

		// Check max age
		if maxAge != 0 && now.Sub(msgs[i].ParsedCreatedAt) > maxAge {
			continue
		}

		// Check min age
		if minAge != 0 && now.Sub(msgs[i].ParsedCreatedAt) < minAge {
			continue
		}

		// Check if pinned message to ignore
		if pinFilterEnable {
			if _, found := pinnedMessages[msgs[i].ID]; found {
				continue
			}
		}

		// Continue only if current msg ID is < toID
		if toID > msgs[i].ID {
			continue
		}

		// Check whether to ignore messages without attachments
		if attachmentFilterEnable && len(msgs[i].Attachments) == 0 {
			continue
		}

		toDelete = append(toDelete, msgs[i].ID)

		//log.Println("Deleting", msgs[i].ContentWithMentionsReplaced())
		if len(toDelete) >= deleteNum || len(toDelete) >= 100 {
			break
		}
	}

	if len(toDelete) < 1 {
		return 0, nil
	} else if len(toDelete) == 1 {
		err = common.BotSession.ChannelMessageDelete(channelID, toDelete[0])
	} else {
		err = common.BotSession.ChannelMessagesBulkDelete(channelID, toDelete)
	}

	return len(toDelete), err
}

func FindRole(gs *dstate.GuildSet, roleS string) *discordgo.Role {
	parsedNumber, parseErr := strconv.ParseInt(roleS, 10, 64)

	if parseErr == nil {
		// was a number, try looking by id
		r := gs.GetRole(parsedNumber)
		if r != nil {
			return r
		}
	}

	// otherwise search by name
	for _, v := range gs.Roles {
		trimmed := strings.TrimSpace(v.Name)

		if strings.EqualFold(trimmed, roleS) {
			return &v
		}
	}

	// couldn't find the role :(
	return nil
}

func PaginateWarnings(parsed *dcmd.Data) func(p *paginatedmessages.PaginatedMessage, page int) (*discordgo.MessageEmbed, error) {

	return func(p *paginatedmessages.PaginatedMessage, page int) (*discordgo.MessageEmbed, error) {

		var err error
		skip := (page - 1) * 6
		userID := parsed.Args[0].Int64()
		limit := 6

		var result []*WarningModel
		var count int
		err = common.GORM.Table("moderation_warnings").Where("user_id = ? AND guild_id = ?", userID, parsed.GuildData.GS.ID).Count(&count).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return nil, err
		}
		err = common.GORM.Where("user_id = ? AND guild_id = ?", userID, parsed.GuildData.GS.ID).Order("id desc").Offset(skip).Limit(limit).Find(&result).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return nil, err
		}

		if len(result) < 1 && p != nil && p.LastResponse != nil { //Dont send No Results error on first execution
			return nil, paginatedmessages.ErrNoResults
		}

		desc := fmt.Sprintf("**Total :** `%d`", count)
		var fields []*discordgo.MessageEmbedField
		currentField := &discordgo.MessageEmbedField{
			Name:  "⠀", //Use braille blank character for seamless transition between feilds
			Value: "",
		}
		fields = append(fields, currentField)
		if len(result) > 0 {

			for _, entry := range result {

				entry_formatted := fmt.Sprintf("#%d: `%20s` - By: **%s** (%13s) \n **Reason:** %s", entry.ID, entry.CreatedAt.UTC().Format(time.RFC822), entry.AuthorUsernameDiscrim, entry.AuthorID, entry.Message)
				if len([]rune(entry_formatted)) > 900 {
					entry_formatted = common.CutStringShort(entry_formatted, 900)
				}
				entry_formatted += "\n"
				if entry.LogsLink != "" {
					entry_formatted += fmt.Sprintf("> logs: [`link`](%s)\n", entry.LogsLink)
				}

				if len([]rune(currentField.Value+entry_formatted)) > 1023 {
					currentField = &discordgo.MessageEmbedField{
						Name:  "⠀",
						Value: entry_formatted + "\n",
					}
					fields = append(fields, currentField)
				} else {
					currentField.Value += entry_formatted + "\n"
				}
			}

		} else {
			currentField.Value = "No Warnings"
		}

		return &discordgo.MessageEmbed{
			Title:       fmt.Sprintf("Warnings - User : %d", userID),
			Description: desc,
			Fields:      fields,
		}, nil
	}
}
