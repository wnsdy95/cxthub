// Repeatable CPU benchmark for graph evidence/projection, not browser paint.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import { performance } from 'node:perf_hooks';
import { build } from 'esbuild';
const root=fileURLToPath(new URL('..',import.meta.url));
const output=await mkdtemp(path.join(tmpdir(),'cxt-graph-benchmark-'));
try {
  const file=path.join(output,'graph.cjs');
  await build({stdin:{contents:`export {GraphIndex} from './src/graphIndex'; export {projectBranchGraph,visibleBranchGraph} from './src/graphProjection'; export {completedBranchEvidence} from './src/graphEvidence'; export {layoutGraph} from './src/graph'; export {previousProgressGroups} from './src/contextHistory';`,resolveDir:root},bundle:true,platform:'node',format:'cjs',outfile:file});
  const {GraphIndex,projectBranchGraph,visibleBranchGraph,completedBranchEvidence,layoutGraph,previousProgressGroups}=createRequire(import.meta.url)(file);
  const at=n=>new Date(1750000000000+n*1000).toISOString();
  const s=(id,parents,n,branch='main')=>({id,repo_id:'repo',doc_hash:id,branch,parents,created_at:at(n),provider:'codex',fidelity:'full'});
  const chain=Array.from({length:10_000},(_,n)=>s(`s${n}`,n?[`s${n-1}`]:[],n)).reverse();
  const cases=[{name:'10k chain',snapshots:chain,refs:[{repo_id:'repo',kind:'branch',name:'main',target:'s9999'}],history:[],reflog:[]}];
  const history=[];
  for(let n=0;n<300;n++) {
    const branch=`feature/${n}`,branch_id=`b${n}`;
    history.push({id:`birth${n}`,repo_id:'repo',kind:'birth',branch,branch_id,source:'s500',target:'s500',created_at:at(10001+n*2)},
      {id:`merge${n}`,repo_id:'repo',kind:'pr-merge',branch:'main',branch_id:'main',source_branch_id:branch_id,source:'s0',shared_target:'s500',target:'s500',pr_completed:true,pr:{number:n+1,head_branch:branch},created_at:at(10002+n*2)});
  }
  cases.push({...cases[0],name:'10k chain + 300 PRs',history});
  const wide=[s('base',[],0)],refs=[];
  for(let b=0;b<100;b++) {
    for(let n=0;n<100;n++) wide.push(s(`${b}-${n}`,[n?`${b}-${n-1}`:'base'],n*100+b+1,`feature/${b}`));
    refs.push({repo_id:'repo',kind:'branch',name:`feature/${b}`,target:`${b}-99`});
  }
  cases.push({name:'10k nodes / 100 lanes',snapshots:wide.reverse(),refs,history:[],reflog:[]});
  cases.push({...cases[0],name:'10k chain / 100 overlapping rewinds',refs:[{...cases[0].refs[0],target:'s99'}],
    reflog:Array.from({length:100},(_,n)=>({kind:'branch',name:'main',old:`s${5049+n*50}`,new:'s99',created_at:at(11000+n)}))});
  const median=values=>[...values].sort((a,b)=>a-b)[Math.floor(values.length/2)];
  for(const c of cases) {
    const runs=[];
    for(let run=0;run<3;run++) {
      const start=performance.now(),index=new GraphIndex(c.snapshots);
      const evidence=completedBranchEvidence(c.snapshots,c.history,index);
      const groups=previousProgressGroups(c.refs,c.snapshots,c.reflog,c.history,undefined,index);
      const t1=performance.now();
      const projection=projectBranchGraph(c.snapshots,c.refs,c.history,c.reflog,c.refs[0].target,c.refs[0].name,index,evidence);
      const t2=performance.now(),layout=layoutGraph(projection.snapshots,projection.pinHead),t3=performance.now();
      const stats=JSON.stringify(index.stats);
      for(let mask=0;mask<8;mask++) {
        const ids=new Set(c.snapshots.filter((_,n)=>(n%8&mask)===0).map(s=>s.id));
        visibleBranchGraph(projection,ids);
      }
      const t4=performance.now();
      if(JSON.stringify(index.stats)!==stats) throw Error('visibility must not query ancestry');
      if(index.stats.cachedMemberships>index.cacheBudget) throw Error('cache exceeds membership budget');
      if(layout.issues.length) throw Error('benchmark generated invalid projection');
      runs.push({evidenceAndGroupsMs:t1-start,projectionMs:t2-t1,layoutMs:t3-t2,eightVisibilityChangesMs:t4-t3,rows:layout.rows.length,lanes:layout.laneCount,groups:groups.length,cachedMemberships:index.stats.cachedMemberships});
    }
    const result={name:c.name};
    for(const key of Object.keys(runs[0])) result[key]=Math.round(median(runs.map(r=>r[key]))*10)/10;
    console.log(JSON.stringify(result));
  }
} finally { await rm(output,{recursive:true,force:true}); }
