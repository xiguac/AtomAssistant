package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/xiguac/atomassistant/internal/device"
	"github.com/xiguac/atomassistant/internal/store/pg"
)

// deviceCmd 处理 atomctl device 的子命令：看手机上报了什么。
func deviceCmd(ctx context.Context, st *pg.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("device 需要子命令: list / data / battery")
	}
	switch args[0] {
	case "list":
		devs, err := st.ListDevices(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\t名称\t机型\t系统\t状态\t最近在线")
		for _, d := range devs {
			state := "等待配对"
			switch {
			case !d.RevokedAt.IsZero():
				state = "已吊销"
			case d.Paired():
				state = "已配对"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, d.Model, d.OS, state, d.LastSeenAt.Local().Format("01-02 15:04:05"))
			stats, err := st.DataStats(ctx, d.ID)
			if err != nil {
				return err
			}
			for _, k := range stats {
				fmt.Fprintf(w, "\t  %s\t%d 条\t最新 %s\t\t\n", k.Kind, k.Count, k.Latest.Local().Format("01-02 15:04:05"))
			}
		}
		return w.Flush()
	case "battery":
		// atomctl device battery [行程小时数] [目标电量百分比]：看服务器学到的充电与掉电速度和预估。
		var req device.BatteryRequest
		if len(args) > 1 {
			h, err := strconv.ParseFloat(args[1], 64)
			if err != nil || h < 0 {
				return fmt.Errorf("行程小时数要是非负数")
			}
			req.TripHours = h
		}
		if len(args) > 2 {
			p, err := strconv.ParseFloat(args[2], 64)
			if err != nil || p < 0 || p > 100 {
				return fmt.Errorf("目标电量要在 0 到 100 之间")
			}
			req.TargetPercent = p
		}
		rep, err := (&device.Service{Store: st}).Battery(ctx, "", req)
		if err != nil {
			return err
		}
		out, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	case "data":
		if len(args) < 2 {
			return fmt.Errorf("data 需要一个数据种类，如 probe.report")
		}
		limit := 1
		if len(args) > 2 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n < 1 {
				return fmt.Errorf("条数要是正整数")
			}
			limit = n
		}
		rows, err := st.QueryData(ctx, device.Query{Kind: args[1], Limit: limit})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("没有 %s 的数据", args[1])
		}
		for _, r := range rows {
			fmt.Printf("# %s  采集于 %s\n%s\n", r.Kind, r.CapturedAt.Local().Format("2006-01-02 15:04:05"), r.Payload)
		}
		return nil
	}
	return fmt.Errorf("未知的 device 子命令 %q（可用：list / data / battery）", args[0])
}
