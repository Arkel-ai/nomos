#!/usr/bin/env python3
"""Regenerate code_du_travail.json — the offline employment-law fixture corpus.

The corpus text is LEGI (Code du travail, LEGITEXT000006072050) under Licence
Ouverte / Open Licence 2.0, published by DILA / Legifrance. Reuse including
commercial reuse is permitted WITH ATTRIBUTION, which is why the generated file
carries a licence header and this script must never strip it.

The snapshot is taken from @socialgouv/legi-data, a LEGI redistribution on the
public npm registry, because PISTE credentials are not available offline. Two
prerequisite steps, run from a scratch directory:

    curl -sLO https://registry.npmjs.org/@socialgouv/legi-data/-/legi-data-2.558.0.tgz
    shasum -a 256 legi-data-2.558.0.tgz        # must match SNAPSHOT_SHA256 below
    tar xzf legi-data-2.558.0.tgz package/data/LEGITEXT000006072050.json
    python3 -c "import json;d=json.load(open('package/data/LEGITEXT000006072050.json'));\
      arts=[];\
      w=lambda n,p:(arts.append((n['data'],p)) if n['type']=='article' else \
        [w(c,p+[n['data'].get('title','').strip()] if n['data'].get('title','').strip() else p) for c in n.get('children',[])]);\
      w(d,[]);\
      json.dump([{'d':a,'p':p} for a,p in arts if a.get('etat')=='VIGUEUR'],open('flat.json','w'))"

then `python3 generate.py` in that directory.

Scope is EMPLOYMENT LAW ONLY (docs/PLAN.md 5.3): the article families of the
individual employment dispute, capped per family. Widening it is a deliberate
decision, not a convenience — all of LEGI is ~10^7 vectors and 80-150 GB.
"""

import json, re, hashlib, collections

v = json.load(open('flat.json'))
FAMS = ['L1132','L1152','L1153','L1221','L1222','L1225','L1226','L1231','L1232','L1233',
        'L1234','L1235','L1237','L1242','L1243','L1245','L1332','L1411','L1451','L1453',
        'L1454','L1471','L3121','L3123','L3141','L3221','L3245','L3251',
        'R1234','R1452','R1454','R1455','R1461']
CAP = 12

def natkey(num):
    # L1233-3 → sortable
    parts = re.findall(r'\d+|[A-Za-z]+', num)
    return tuple((int(p) if p.isdigit() else p) for p in parts)

buckets = collections.defaultdict(list)
for a in v:
    d = a['d']
    num = (d.get('num') or '').replace(' ', '')
    txt = (d.get('texte') or '').strip()
    if not num or len(txt) < 40:
        continue
    for f in FAMS:
        if num.startswith(f):
            buckets[f].append((num, d, a['p']))
            break

def ms_to_unix(ms):
    if ms is None:
        return None
    s = int(ms) // 1000
    # 2999-01-01 is LEGI's "no end" sentinel
    if s >= 32472144000:
        return None
    return s

out = []
for f in FAMS:
    rows = sorted(buckets[f], key=lambda r: natkey(r[0]))
    for num, d, path in rows[:CAP]:
        text = re.sub(r'[ \t]+', ' ', (d['texte'] or '').strip())
        text = re.sub(r'\n{3,}', '\n\n', text)
        out.append({
            "document_id": d['cid'],
            "version_id": d['id'],
            "number": num,
            "citation_label": "C. trav., art. " + num,
            "section_path": [p for p in path if p][1:],
            "state": d.get('etat'),
            "valid_from": ms_to_unix(d.get('dateDebut')),
            "valid_to": ms_to_unix(d.get('dateFin')),
            "canonical_url": "https://www.legifrance.gouv.fr/codes/article_lc/" + d['id'],
            "nota": (d.get('nota') or '').strip() or None,
            "text": text,
            "text_sha256": hashlib.sha256(text.encode()).hexdigest(),
        })

out.sort(key=lambda r: natkey(r['number']))

doc = {
    "schema_version": 1,
    "corpus_id": "code_du_travail_contentieux_prudhomal_v1",
    "description": (
        "Offline employment-law fixture corpus for Nomos. A curated subset of the French "
        "Code du travail covering the individual employment dispute (formation and "
        "execution of the contract, discrimination and harcelement, licenciement pour "
        "motif personnel and economique, rupture conventionnelle, CDD, working time, "
        "paid leave, pay claims, prescription, and the conseil de prud'hommes procedure). "
        "It exists so the app and the relevance evaluations run with NO network access and "
        "NO PISTE credentials. It is a fixture, NOT an authoritative legal snapshot: "
        "consolidated LEGI text is a cache, the opposable text is the authenticated "
        "Legifrance publication, and every citation must be revalidated against the "
        "official source before export (docs/PLAN.md 5.1, 6.4)."
    ),
    "license": {
        "code": "etalab-2.0",
        "name": "Licence Ouverte / Open Licence version 2.0",
        "url": "https://www.etalab.gouv.fr/licence-ouverte-open-licence/",
        "attribution": "Code du travail — Direction de l'information legale et administrative (DILA) / Legifrance, Licence Ouverte 2.0. Reuse is permitted, including commercially, provided the source and its last update date are cited.",
        "source_url": "https://www.legifrance.gouv.fr/codes/texte_lc/LEGITEXT000006072050",
        "reuse_conditions": "Attribution required. The licence does not lift personal-data or third-party intellectual-property obligations.",
    },
    "source": {
        "corpus": "LEGI (consolidated legislation)",
        "text_id": "LEGITEXT000006072050",
        "text_title": "Code du travail",
        "publisher": "DILA / Legifrance",
        "snapshot": "socialgouv/legi-data 2.558.0 (LEGI redistribution, Licence Ouverte 2.0)",
        "snapshot_sha256": "151796fd1761ca4f3f32726c2754b93f755df2684a483645c67c36892bd53df0",
        "extracted_at": "2026-08-27",
        "selection": "Articles in force (etat=VIGUEUR) from %d article families of the individual employment dispute, capped at %d articles per family and sorted by article number." % (len(FAMS), CAP),
        "families": FAMS,
    },
    "articles": out,
}
json.dump(doc, open('code_du_travail.json','w'), ensure_ascii=False, indent=1)
print('articles', len(out))
print('states', collections.Counter(a['state'] for a in out))
print('chars', sum(len(a['text']) for a in out))
