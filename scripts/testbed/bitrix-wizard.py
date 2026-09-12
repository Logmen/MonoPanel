#!/usr/bin/env python3
# Drives the 1C-Bitrix install wizard end to end over HTTP, the way a browser
# would: regular steps get the field overrides and "Next"; the AJAX steps
# (module installation, updates, demo data) are looped by following the
# Post({...}) hints the wizard returns in each [response]; a radio group with
# no choice takes its first option; an error box is retried, then skipped.
#
#   bitrix-wizard.py http://site '{"__wiz_field": "value", ...}' [max-steps]
#
# Prints one line per wizard step and stops when no wizard form is left
# (the site itself answers) or the wizard gets stuck.
import sys, json, re, urllib.request, urllib.parse, http.cookiejar
from html.parser import HTMLParser
base=sys.argv[1].rstrip('/'); ov=json.loads(sys.argv[2]); limit=int(sys.argv[3]) if len(sys.argv)>3 else 400
cj=http.cookiejar.CookieJar(); op=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
class P(HTMLParser):
    def __init__(s): super().__init__(); s.fields=[]; s.forms=[]; s.text=[]; s.sel={}; s.ajax=False; s.script=[]; s.radios_js=[]
    def handle_starttag(s,tag,attrs):
        a=dict(attrs)
        if tag=='form': s.forms.append(a.get('action'))
        if tag in('input','select','textarea','button'):
            s.fields.append((tag,a.get('type',''),a.get('name'),a.get('value'),'checked' in a))
            if a.get('type')=='radio' and a.get('value') is None: s.radios_js.append(a.get('onclick') or a.get('onchange') or '')
        if tag=='option' and 'selected' in a and s.fields and s.fields[-1][0]=='select': s.sel[s.fields[-1][2]]=a.get('value')
    def handle_data(s,d):
        if 'new CAjaxForm' in d: s.ajax=True
        d=d.strip()
        if d and len(d)<300 and not d.startswith(('function','var ','PreloadImages','window.','if(','document.','#noscript','div {')): s.text.append(d)
def req(url,data=None):
    r=op.open(urllib.request.Request(url,data=urllib.parse.urlencode(data).encode() if data is not None else None,headers={'User-Agent':'Mozilla/5.0'}),timeout=900)
    return r.geturl(), r.read().decode('utf-8','replace')
url,html=req(base+'/'); last=None; same=0; n=0
while n<limit:
    n+=1
    p=P(); p.feed(html)
    form={}; cur=None; visible=[]; names=[f[2] for f in p.fields]
    for tag,typ,name,val,chk in p.fields:
        if not name: continue
        if name=='CurrentStepID': cur=val
        if tag=='input' and typ in('checkbox','radio') and not chk: continue
        if tag=='button' or typ in('submit','button','image'):
            if name=='StepNext' and not p.ajax: form[name]=val or 'Next'
            continue
        if typ!='hidden': visible.append(name)
        form[name]=val if val is not None else ''
    form.update(p.sel)
    # a radio group with nothing checked (the solution choice): take the first option
    radios={}
    for tag,typ,name,val,chk in p.fields:
        if tag=='input' and typ=='radio' and name: radios.setdefault(name,[]).append((val,chk))
    for name,opts in radios.items():
        if not any(c for _,c in opts) and name not in form and opts[0][0] is not None:
            form[name]=opts[0][0]; print("    picked %s=%s of %s" % (name, opts[0][0], [v for v,_ in opts]))
    if p.radios_js and cur!=last:
        # value-less radios select through JavaScript: replay the first one's assignments
        js=p.radios_js[0]; print("    radio onclick:", js[:300])
        for kk,vv in re.findall(r"""(?:elements\[|getElementsByName\(|getElementById\()['"]([^'"]+)['"]\)?\]?(?:\[0\])?\.value\s*=\s*['"]([^'"]*)['"]""",js):
            form[kk]=vv; print("    set %s=%s" % (kk,vv))
        open('/root/bx_%s.html'%cur,'w').write(html)
    for k,v in ov.items():
        if k in names: form[k]=v
    errs=[t for t in p.text if re.search(r'(^Не |не удалось|[Оо]шибк|Error|error|failed)',t) and 'произошла ошибка установки' not in t and 'display_errors' not in t and 'Повторите установку' not in t]
    if cur!=last:
        print("[%d] step=%s%s fields=%s" % (n,cur,' (ajax)' if p.ajax else '',visible)); last=cur; same=0
        if errs: print("    errors:", errs[:4])
    else:
        same+=1
        if same<=2 and errs: print("    errors on %s: %s" % (cur, errs[:4]))
        if same==2: print("    text:", " | ".join(p.text[-24:])[:700])
        if same>25: print("stuck at", cur); break
    if not p.forms or cur is None:
        print("no wizard form; url=%s title/text: %s" % (url, " | ".join(p.text)[:400]))
        break
    action=urllib.parse.urljoin(url,p.forms[0] or url)
    if p.ajax:
        # loop the AJAX sub-steps until the response stops asking for another
        k=0; retries=0
        # the page tells which hidden fields carry the AJAX step and stage
        mm=re.search(r"new CAjaxForm\([^{]*\{(.*?)\}",html,re.S)
        hmap=dict(re.findall(r'"(\w+)"\s*:\s*"([^"]+)"',mm.group(1))) if mm else {}
        while True:
            k+=1
            _,resp=req(action,form)
            m=re.search(r"Post\(\s*\{(.*?)\}",resp,re.S)
            mp=re.search(r"Post\(\s*'([^']*)'\s*,\s*'([^']*)'",resp)
            if not m and mp:
                # the solution wizard passes the step positionally: Post(step, stage, status)
                m=type('M',(),{'group':lambda self,i,a=mp.group(1),b=mp.group(2): "'nextStep': '%s', 'nextStepStage': '%s'" % (a,b)})()
            st=re.search(r"SetStatus\('?(\d+)'?(?:,\s*'([^']*)')?",resp)
            new=dict(re.findall(r"'(\w+)'\s*:\s*'?([^',}\s]*)'?",m.group(1))) if m else {}
            if m and any(form.get(hmap.get(kk,'__wiz_'+kk))!=vv for kk,vv in new.items()):
                for kk,vv in new.items():
                    form[hmap.get(kk,'__wiz_'+kk)]=vv
                if k%10==1 or (st and st.group(2)): print("    ajax %d: %s%% %s -> %s/%s" % (k, st.group(1) if st else '?', (st.group(2) if st and st.group(2) else '')[:50], form.get('__wiz_nextStep'), form.get('__wiz_nextStepStage')))
                if k>600: print("ajax loop too long"); sys.exit(1)
                continue
            if '<html' in resp.lower():
                # the wizard re-rendered its page: the next step, or an error box with retry/skip
                e=re.search(r'id="error_text"[^>]*>\s*(\S.*?)</div>',resp,re.S)
                q=P(); q.feed(resp)
                cur2=next((f[3] for f in q.fields if f[2]=='CurrentStepID'),None)
                if e and cur2==cur:
                    err=re.sub(r'<[^>]+>','',e.group(1)).strip()
                    retries=retries+1 if 'retries' in dir() else 1
                    print("    ajax %d: error on %s: %s (%s)" % (k, cur, err[:200], 'retry' if retries<3 else 'skip'))
                    if retries>=3 and form.get('__wiz_nextStep')!='main': form['__wiz_nextStepStage']='skip'
                    if retries>6: print("giving up on", cur); sys.exit(1)
                    continue
                print("    ajax %d: page rendered, step %s" % (k, cur2))
                html=resp; break
            body=re.sub(r'\s+',' ',resp)
            print("    ajax %d final response: %s" % (k, body[:400]))
            for kk,vv in re.findall(r"""elements\[['"]([^'"]+)['"]\]\.value\s*=\s*['"]([^'"]*)['"]""",resp): form[kk]=vv
            if 'submit()' in resp or 'StopAjax' in resp:
                form.pop('__wiz_nextStep',None); form.pop('__wiz_nextStepStage',None)
                url,html=req(action,form)
                break
            print("unexpected ajax response, stopping"); sys.exit(1)
        continue
    url,html=req(action,form)
print("final url:", url)
