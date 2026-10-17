// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

// oauth_pool.go 扫码登录的「规范化落库」（v0.31.37）。
//
// ## 为什么要这一层
//
// 此前扫码登录与导入 JSON 走的是两条**不等价**的落库路径：
//
//	扫码：PollOAuth → Account{uid,nickname,realm,tokens} → UpsertAccount
//	导入：parseDesktopAccounts → Account{…,device_token,enterpriseId,edition…}
//
// 差在三处，而三处都会在运行期变成"掉线"或"说不清"：
//
//  1. **device_token 为空**。运行期靠 `device_token_fallback=derive` 现算一个派生值，
//     于是「库里有什么」与「实际发出去什么」永远对不上 —— 导出再导入、迁移、审计
//     都拿不到真实值。现在扫码成功就把派生值**显式写进库**（与运行期同源同值），
//     导出即所见。
//  2. **不登记来源**。运营判断"扫码登录更容易掉线"，但库里没有任何列能区分来源，
//     这条判断既不能证实也不能证伪。现在写 `accounts.source='qr'`。
//  3. **代挂号不入池**。扫码后停在 `pool_state=”`（未申请），要人工点"申请入池" →
//     管理员审批。用户要求的语义是"扫完就在池里"，故按 kv `qr_auto_pool`（默认开）
//     自动置 `in`，并写清备注。
//
// ## 边界（必须写清楚，否则下一个人会以为扫码号"什么都自动"）
//
//   - 自动入池**不改归属**：仍写 owner_user_id = 扫码的人，平台号（owner=0）语义不变。
//   - 自动入池受 kv 开关控制，关掉即回到"需申请"的旧行为（可回滚）。
//   - 扫码**不自动分配出口**：出口是运维决策（默认出口/直连/指定线路），
//     自动挑一条会制造"同一个号从两个 IP 出去"的更严重问题。
//   - 扫码**不主动刷新 token**：见 docs/同类项目比对-20261018.md 的对照实验，
//     号主本机客户端与服务器共用同一条 refresh 轮换链，谁先刷谁把对方打掉。
package main

import (
	"encoding/json"
	"log"
	"time"

	"workbuddy2api/internal/upstream"
)

// qrAutoPoolKey 扫码自动入池开关（kv）。缺省/非法值按"开"处理。
const qrAutoPoolKey = "qr_auto_pool"

// qrAutoPoolOn 读开关。默认开：用户明确要求"扫码后自动进号池"。
func (a *app) qrAutoPoolOn() bool {
	v, err := a.store.GetKV(qrAutoPoolKey)
	if err != nil || v == "" {
		return true
	}
	return v != "0" && v != "false"
}

// materializeQRLogin 把一次成功的扫码登录**规范成与导入同构的账号行**。
//
// 返回的 authJSON 是与导出文件同构的单行 JSON（顶层数组里的一项），
// 前端可以直接让用户下载/复制 —— 这就是"扫码后自动转 JSON"的落地形态：
// 不是额外生成一个文件，而是让**库里的行**与**导出的行**是同一套字段。
//
// 幂等：重复扫码同一账号不会改写 source（首次来源）与 pool_state（业务状态）。
func (a *app) materializeQRLogin(acc *Account) (string, error) {
	if acc == nil || acc.UID == "" {
		return "", nil
	}
	now := time.Now().Unix()

	// 1) 来源。只在"空 → qr"时写，避免把 import 号洗成 qr。
	if err := a.store.SetAccountSource(acc.UID, sourceQR); err != nil {
		log.Printf("WARN: [qr] 写来源失败 uid=%s: %v", acc.UID, err)
	}

	// 2) device_token 显式落库（与运行期派生同源同值）。
	//    只在为空时补：已经有真实设备凭证的号（导入带来的）不该被派生值覆盖。
	if acc.DeviceToken == "" {
		dt := upstream.AccountDeviceToken(acc.UID)
		if dt != "" {
			if err := a.store.SetAccountDeviceToken(acc.UID, dt); err != nil {
				log.Printf("WARN: [qr] 写设备凭证失败 uid=%s: %v", acc.UID, err)
			} else {
				acc.DeviceToken = dt
				acc.DeviceTokenAt = now
			}
		}
	}

	// 3) 自动入池（仅代挂号需要；平台号 owner=0 恒在池）。
	if acc.OwnerUserID > 0 && a.qrAutoPoolOn() {
		cur, err := a.store.GetAccount(acc.UID)
		if err == nil && cur != nil && cur.PoolState == "" {
			note := "扫码登录自动入池"
			if err := a.store.SetAccountPoolState(acc.UID, "in", note); err != nil {
				log.Printf("WARN: [qr] 自动入池失败 uid=%s: %v", acc.UID, err)
			} else {
				acc.PoolState = "in"
				acc.HostNote = note
				log.Printf("INFO: [qr] %s 扫码后自动入池（kv %s 开启）", acc.UID, qrAutoPoolKey)
			}
		}
	}

	// 4) 生成与导出同构的 JSON（顶层数组，一项），供前端一键下载。
	row := buildExportRowsWithSource([]*Account{acc}, now, "wb2api/"+version+" qr-login",
		map[string]string{acc.UID: sourceQR})
	raw, err := json.MarshalIndent(row, "", "  ")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
