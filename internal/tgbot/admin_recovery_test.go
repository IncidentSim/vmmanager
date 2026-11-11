package tgbot

import (
 "context"
 "encoding/json"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"

 "github.com/petruekhin/vibe-firecracker/controlclient"
)

func TestAdminRunResumesSnapshotAndDeliversOnceAcrossRestart(t *testing.T) {
 dir:=privateDir(t)
 priv,_:=writeKeys(t,t.TempDir())
 token:=strings.Repeat("x",43)
 advances:=0
 srv:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  batch:=controlclient.AdminBatch{Token:token,AdminUserID:42,ChatID:42,Confirmed:true,VMs:[]controlclient.AdminVMRef{{ID:"vm-before",OwnerUserID:43}},Pending:1}
  if advances>0 {batch.Pending=0;batch.Completed=1}
  if r.Method=="GET" && r.URL.Path=="/v1/admin/delete-all" {json.NewEncoder(w).Encode(map[string]any{"batches":[]controlclient.AdminBatch{batch}});return}
  if r.Method!="POST" || r.URL.Path!="/v1/admin/delete-all/"+token {t.Errorf("restart broadened inventory: %s %s",r.Method,r.URL.Path)}
  advances++;batch.Pending=0;batch.Completed=1;w.WriteHeader(202);json.NewEncoder(w).Encode(batch)
 }));defer srv.Close()
 cfg:=Config{AllowedUserIDs:[]int64{43},AdminUserIDs:[]int64{42},ImageID:"ubuntu",APIURL:srv.URL,JWT:&JWTConfig{PrivateKeyFile:priv,Issuer:jIss,Audience:jAud}}
 for attempt:=0;attempt<2;attempt++ {
  d,e:=OpenDelivery(dir);if e!=nil {t.Fatal(e)}
  ctx,cancel:=context.WithCancel(context.Background())
  tg:=&fakeTelegram{onPoll:func(int64){cancel()}}
  bot,e:=NewBot(cfg,tg,nil,d);if e!=nil {t.Fatal(e)}
  if e=bot.Run(ctx);e!=nil {t.Fatal(e)}
  cancel();d.Close()
  expected:=1;if attempt==1 {expected=0}
  if len(tg.messages)!=expected {t.Fatalf("attempt %d: messages %+v",attempt,tg.messages)}
 }
 if advances!=1 {t.Fatalf("restart duplicated or broadened snapshot: %d advances",advances)}
}
