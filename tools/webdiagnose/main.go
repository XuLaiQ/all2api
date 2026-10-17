// Command webdiagnose prints credential-free local diagnostics for Web adapters.
package main

import (
 "context"
 "database/sql"
 "encoding/base64"
 "encoding/json"
 "fmt"
 "os"
 "sort"
 "strings"
 "time"

 "github.com/XuLaiQ/all2api/internal/config"
 "github.com/XuLaiQ/all2api/internal/adapters/chatgpt"
 "github.com/XuLaiQ/all2api/internal/ports"
 cryptoinfra "github.com/XuLaiQ/all2api/internal/infrastructure/crypto"
 "github.com/XuLaiQ/all2api/internal/infrastructure/persistence"
 _ "modernc.org/sqlite"
)

func main() {
 if _, err := config.LoadEnvFile(); err != nil { panic("cannot load environment") }
 cfg, err := config.Load(); if err != nil { panic("invalid configuration") }
 db, err := sql.Open("sqlite", cfg.DBPath); if err != nil { panic("cannot open database") }; defer db.Close()
 ctx := context.Background()
 // Only safe columns are selected; account names and prompt content are excluded.
 rows, err := db.QueryContext(ctx, `SELECT a.channel, a.native_id, a.enabled, COALESCE(a.status_override,a.status), COALESCE(r.last_status,0), COALESCE(r.last_error_kind,'') FROM accounts a LEFT JOIN account_runtime_state r ON r.account_id=a.id WHERE a.channel IN ('chatgpt','doubao','grok')`)
 if err != nil { panic("cannot query accounts") }
 type account struct { channel, id, status, kind string; enabled, last int }
 var accounts []account
 for rows.Next() { var a account; if err := rows.Scan(&a.channel,&a.id,&a.enabled,&a.status,&a.last,&a.kind); err != nil { panic("scan failed") }; accounts=append(accounts,a) }; rows.Close()
 // Resolve encrypted credentials in memory, never print their values.
 storage, err := persistence.Open(ctx,cfg.DBPath); if err != nil { panic("cannot initialize storage") }; defer storage.Close()
 box := cryptoinfra.NewFernet(cfg.CredentialMasterKey)
 for i,a := range accounts {
  fmt.Printf("account=%d channel=%s enabled=%d state=%s last_status=%d last_kind=%s\n",i,a.channel,a.enabled,a.status,a.last,a.kind)
  cred,err := storage.ReadCredential(ctx,a.channel,a.id,box); if err != nil { fmt.Println("credential_read=failed"); continue }
  keys:=make([]string,0,len(cred)); for k,v:=range cred { if strings.TrimSpace(v)!="" { keys=append(keys,k) } }; sort.Strings(keys); fmt.Printf("credential_fields=%v\n",keys)
  token:=cred["access_token"]; if token=="" { token=cred["token"] }
  parts:=strings.Split(token,"."); if len(parts)==3 { raw,err:=base64.RawURLEncoding.DecodeString(parts[1]); if err==nil { var claims struct { Exp int64 `json:"exp"`; Aud any `json:"aud"`; Iss string `json:"iss"` }; if json.Unmarshal(raw,&claims)==nil { fmt.Printf("token_expired=%t expires_at=%s issuer=%s audience=%v\n", claims.Exp>0 && claims.Exp<time.Now().Unix(),time.Unix(claims.Exp,0).UTC().Format(time.RFC3339),claims.Iss,claims.Aud) } } }
 }
 fmt.Printf("chatgpt_proxy_configured=%t grok_proxy_configured=%t\n", cfg.ChatGPTProxy!="",cfg.GrokProxy!="")
 // Aggregate error categories without exposing stored messages or upstream bodies.
 logs,err:=db.QueryContext(ctx,`SELECT channel,status,error_kind,COUNT(*) FROM request_logs WHERE channel IN ('chatgpt','doubao','grok') GROUP BY channel,status,error_kind ORDER BY channel,status`)
 if err==nil { defer logs.Close(); for logs.Next() { var c,k string; var s,n int; if logs.Scan(&c,&s,&k,&n)==nil { fmt.Printf("logs channel=%s status=%d kind=%s count=%d\n",c,s,k,n) } } }
 if len(os.Args)>1 && os.Args[1]=="chatgpt" {
  client:=chatgpt.NewClientWithProxy(cfg.ChatGPTWebBase,cfg.ChatGPTProxy,func(ctx context.Context,id string)(map[string]string,error){return storage.ReadCredential(ctx,"chatgpt",id,box)})
  client.EnableTokenRefresh(chatgpt.NewOAuthClient(cfg.ChatGPTPlatformBase,cfg.ChatGPTProxy),func(ctx context.Context,id string,c map[string]string)error{return storage.UpdateCredential(ctx,"chatgpt",id,c,box,ports.AuditEvent{Actor:"system",Action:"refresh_credential"})})
  // Select a nonexpired token first to separate Web transport from refresh failures.
  for _,a:=range accounts { if a.channel!="chatgpt" || a.enabled==0 {continue}; cred,err:=storage.ReadCredential(ctx,a.channel,a.id,box); if err!=nil {continue}; token:=cred["access_token"]; parts:=strings.Split(token,"."); var claims struct{Exp int64 `json:"exp"`}; if len(parts)==3 {raw,_:=base64.RawURLEncoding.DecodeString(parts[1]); _=json.Unmarshal(raw,&claims)}; if claims.Exp<time.Now().Unix(){continue}
   probeCtx,cancel:=context.WithTimeout(ctx,100*time.Second)
   models,err:=client.ModelsWithAccount(probeCtx,a.id); fmt.Printf("probe_chatgpt_models count=%d error=%v\n",len(models),err)
   if err==nil && len(models)>0 { result,chatErr:=client.Chat(probeCtx,ports.ChatInput{Model:models[0].UpstreamID,Messages:[]ports.ChatMessage{{Role:"user",Content:"Reply with OK."}}},a.id); fmt.Printf("probe_chatgpt_chat text_length=%d error=%v\n",len(result.Text),chatErr) }
   cancel(); break
  }
 }
}
