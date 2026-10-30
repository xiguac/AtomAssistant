package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/xiguac/atomassistant/internal/config"
	"github.com/xiguac/atomassistant/internal/connector/holiday"
	"github.com/xiguac/atomassistant/internal/connector/mail"
	"github.com/xiguac/atomassistant/internal/store/pg"
	"github.com/xiguac/atomassistant/internal/vault"
)

// connectorCmd 处理 atomctl connector 的子命令。
func connectorCmd(st *pg.Store, cfg *config.Config, args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("connector 需要子命令: check")
	}
	file, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("未设置 ATOM_CONFIG，不知道要检查哪些连接器")
	}
	safe, err := vault.Open(st.Vault(), cfg.VaultKey)
	if err != nil {
		return err
	}
	// 真实的邮件和日历服务器在公网上，握手加登录比本地数据库慢得多。
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	checked := 0
	cal, err := file.Calendar(safe)
	if err != nil {
		return err
	}
	if cal != nil {
		checked++
		fmt.Printf("日历 %s（%s）\n", cal.Endpoint, cal.Username)
		cals, err := cal.Calendars(ctx)
		if err != nil {
			fmt.Printf("  失败: %v\n", err)
		} else {
			names := make([]string, 0, len(cals))
			for _, c := range cals {
				names = append(names, c.Name)
			}
			fmt.Printf("  登录成功，日历: %s\n", strings.Join(names, "、"))
			now := time.Now()
			events, _, err := cal.Events(ctx, "", now, now.AddDate(0, 0, 7))
			if err != nil {
				fmt.Printf("  读取日程失败: %v\n", err)
			} else {
				fmt.Printf("  未来 7 天有 %d 条日程\n", len(events))
			}
		}
	}

	box, err := file.Mail(safe)
	if err != nil {
		return err
	}
	if box != nil {
		checked++
		fmt.Printf("邮件 %s（%s）\n", box.IMAPAddr, box.Username)
		folders, err := box.Folders(ctx)
		if err != nil {
			fmt.Printf("  失败: %v\n", err)
		} else {
			var names []string
			roles := map[string]bool{}
			for _, f := range folders {
				roles[f.Role] = true
				if f.Role != "" {
					names = append(names, f.Name+"("+f.Role+")")
				} else {
					names = append(names, f.Name)
				}
			}
			fmt.Printf("  登录成功，文件夹: %s\n", strings.Join(names, "、"))
			unread, err := box.Search(ctx, mail.Query{UnreadOnly: true, Limit: mail.MaxResults})
			if err != nil {
				fmt.Printf("  读取收件箱失败: %v\n", err)
			} else if len(unread) == mail.MaxResults {
				fmt.Printf("  收件箱未读不少于 %d 封\n", len(unread))
			} else {
				fmt.Printf("  收件箱未读 %d 封\n", len(unread))
			}
			if !roles["sent"] {
				fmt.Println("  注意: 没认出「已发送」文件夹，助手发出的信无法留底")
			}
			if !roles["trash"] {
				fmt.Println("  注意: 没认出废纸篓，助手无法删除邮件")
			}
		}
		if box.SMTPAddr == "" {
			fmt.Println("  没有配置 smtp，助手只能读和整理邮件，不能发信")
		} else {
			// 发信服务器只有真发一封才验证得了，这里不替用户发。
			fmt.Printf("  发信服务器 %s（未验证：要验证只能真发一封信）\n", box.SMTPAddr)
		}
	}
	feed, err := file.Holidays()
	if err != nil {
		return err
	}
	if feed != nil {
		checked++
		fmt.Printf("节假日 %s\n", feed.URL)
		now := time.Now()
		days, err := feed.Between(ctx, now, now.AddDate(0, 0, 90))
		if err != nil {
			fmt.Printf("  失败: %v\n", err)
		} else {
			fmt.Println("  取到了，未来 90 天里的放假与调休：")
			for _, d := range days {
				switch d.Kind {
				case holiday.KindOff:
					fmt.Printf("    %s ~ %s %s 放假\n", d.Start, d.End, d.Name)
				case holiday.KindWorkday:
					fmt.Printf("    %s %s 调休上班\n", d.Start, d.Name)
				}
			}
		}
	}
	qw, err := file.Weather(safe)
	if err != nil {
		return err
	}
	if qw != nil {
		checked++
		fmt.Printf("天气 和风天气（%s）\n", qw.Host)
		rep, err := qw.Forecast(ctx, "北京", "", 1)
		if err != nil {
			fmt.Printf("  失败: %v\n", err)
		} else {
			fmt.Printf("  取到了，%s 现在 %s，%s\n", rep.Place.Name, rep.Now.Text, rep.Now.Temp)
		}
	}
	if checked == 0 {
		fmt.Println("配置里没有 connectors 一节，没有接入任何连接器")
	}
	return nil
}
