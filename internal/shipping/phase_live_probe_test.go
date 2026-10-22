package shipping
import (
 "context"
 "encoding/json"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "testing"
 "time"
 "github.com/kunchenguid/no-mistakes/internal/types"
)
// The real read-only CLI observes disposable persisted run state. Only the
// external forge is simulated. Oracle: exact duplicates are permitted;
// independent warnings, paraphrases and operator-authored copies are refused.
func TestPhaseLiveNoSurfaceReceipt(t *testing.T) {
 root := t.TempDir()
 t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root,"no-global-config"))
 t.Setenv("GIT_CONFIG_NOSYSTEM","1")
 t.Setenv("GIT_CONFIG_COUNT","0")
 wrapper := "#!/bin/sh\ncase \"$1 $2\" in 'auth status') exit 0;; 'api repos/owner/project/pulls/7') printf '%s\\n' \"$PHASE_PR_FACTS\"; exit 0;; *) printf 'unexpected forge request: %s\\n' \"$*\" >&2; exit 89;; esac\n"
 if err := os.WriteFile(filepath.Join(root,"gh"), []byte(wrapper),0700); err != nil {t.Fatal(err)}
 const canonical = "this change has no live-validatable surface; proceed without live validation? (0 of 1 scenarios were driven live against the product); documentation: Markdown has no running product surface"
 for _, kind := range []string{"exact-duplicates","independent-warning","paraphrase","operator-copy"} {
 t.Run(kind,func(t *testing.T){
 f := newExportFixture(t)
 rows,err:=f.database.GetStepsByRun(f.run.ID); if err != nil {t.Fatal(err)}
 items:=[]types.Finding{{ID:"test-1",Severity:"warning",Action:"ask-user",Description:canonical},{ID:"test-2",Severity:"warning",Action:"ask-user",Description:canonical}}
 switch kind {
 case "independent-warning":items=append(items,types.Finding{ID:"test-3",Severity:"warning",Action:"ask-user",Description:"workflow validation found an unresolved runner-policy decision"})
 case "paraphrase":items[1].Description="Human acknowledgement is needed to proceed without live validation"
 case "operator-copy":items[1].Source=types.FindingSourceUser
 }
 evidence:=types.Findings{Items:items,Verdict:types.TestVerdictNoSurface,Scenarios:[]types.TestScenario{{Name:"documentation",Result:types.ScenarioResultUntested,Reason:"Markdown has no running product surface"}}}
 raw,err:=types.MarshalFindingsJSON(evidence);if err!=nil{t.Fatal(err)}
 if err=f.database.ParkStepForApproval(f.run.ID,rows[3].ID,types.StepStatusAwaitingApproval,0,1,&raw);err!=nil{t.Fatal(err)}
 if err=f.database.SetTestApprovalReason(rows[3].ID,"");err!=nil{t.Fatal(err)}
 if err=f.database.CompleteStep(rows[3].ID,0,1,"");err!=nil{t.Fatal(err)}
 facts:=map[string]any{"number":7,"html_url":f.forge.facts.URL,"state":"open","merged":false,"head":map[string]any{"ref":"feature","sha":f.run.HeadSHA,"repo":map[string]string{"full_name":"owner/project"}},"base":map[string]any{"ref":"main","sha":f.run.BaseSHA,"repo":map[string]string{"full_name":"owner/project"}}}
 body,err:=json.Marshal(facts);if err!=nil{t.Fatal(err)}
 ctx,cancel:=context.WithTimeout(context.Background(),20*time.Second);defer cancel()
 cmd:=exec.CommandContext(ctx,os.Getenv("PHASE_BINARY"),"axi","review-receipt","--run",f.run.ID,"--phase","review")
 cmd.Dir=f.dir
 cmd.Env=append(os.Environ(),"NM_HOME="+filepath.Dir(f.dir),"HOME="+root,"XDG_CONFIG_HOME="+root,"PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"),"NO_MISTAKES_TELEMETRY=off","NO_MISTAKES_NO_UPDATE_CHECK=1","PHASE_PR_FACTS="+string(body))
 output,runErr:=cmd.CombinedOutput()
 status:=0;if runErr!=nil{if e,ok:=runErr.(*exec.ExitError);ok{status=e.ExitCode()}else{t.Fatal(runErr)}}
 transcript:="Input findings: "+raw+"\n$ no-mistakes axi review-receipt --run <disposable-run> --phase review\n"+string(output)+"\nexit="+fmtInt(status)+"\n"
 if err=os.WriteFile(filepath.Join(os.Getenv("PHASE_EVIDENCE"),"receipt-"+kind+".txt"),[]byte(transcript),0600);err!=nil{t.Fatal(err)}
 t.Log(transcript)
 if kind=="exact-duplicates" {
 var p Payload
 if runErr!=nil || json.Unmarshal(output,&p)!=nil || p.Phase!="review" || p.Steps[3].Status!="completed" || p.TestOverride!=nil {t.Fatalf("exact duplicate receipt failed: %v %s",runErr,output)}
 }else if runErr==nil || !strings.Contains(string(output),"shipping step is incomplete, skipped, or overridden: test") {t.Fatalf("invalid evidence received receipt: %v %s",runErr,output)}
 })
 }
}
func fmtInt(n int) string {if n==0{return "0"};if n==1{return "1"};return "unexpected"}
