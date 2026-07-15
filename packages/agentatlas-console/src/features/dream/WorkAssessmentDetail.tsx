import type { AssessmentDimensionDetail, WorkAssessmentDetail as WorkAssessmentDetailPayload } from "../../api/assessments";
import "./dream.css";

// WorkAssessmentDetail is the MANAGEMENT assessment detail (Task 18D): a pure
// presentational view reached from existing work-memory/Dream context or the
// Atlas Assistant (the Console layout/navigation is frozen — this adds no new
// route). It renders the manager-authorized detail INCLUDING the score
// (satisfied outcomes) and level (confidence) and the manager notes — but ONLY
// when the payload carries them. Given a score-free payload (an employee-shaped
// projection) it renders the facts and NO score/level/notes, so the score can
// never leak through this view. It exposes no HR-action control.
export function WorkAssessmentDetail({ assessment }: { assessment: WorkAssessmentDetailPayload }) {
  return (
    <main className="dream-page">
      <header className="dream-page-header">
        <div>
          <p className="console-route-kicker">工作评估</p>
          <h1>员工工作评估详情</h1>
          <p>{`员工 ${assessment.subject} 在 ${formatTime(assessment.period.start)} 至 ${formatTime(assessment.period.end)} 的评估。`}</p>
        </div>
      </header>
      <div className="dream-immutable">
        <strong>评估结果不可直接修改</strong>
        <span>员工可提交更正，更正会生成新版本；评估结果不会直接触发任何人事动作。</span>
      </div>
      <div className="dream-run-grid">
        {assessment.dimensions.map((dimension, index) => (
          <DimensionCard key={`${dimension.dimension}-${index}`} dimension={dimension} />
        ))}
      </div>
      {assessment.narrative ? (
        <section className="glass-rest wa-notes">
          <h2>主管备注</h2>
          <p>{assessment.narrative}</p>
        </section>
      ) : null}
      {assessment.manager?.manager ? (
        <p className="dream-meta-label wa-manager">确认主管：{assessment.manager.manager}</p>
      ) : null}
    </main>
  );
}

function DimensionCard({ dimension }: { dimension: AssessmentDimensionDetail }) {
  return (
    <section className="glass-rest">
      <h2>{dimension.dimension}</h2>
      {dimension.state === "not_assessable" ? (
        <p className="dream-meta-label">数据不足：{dimension.not_assessable_reason || "缺少可核验证据"}</p>
      ) : null}
      {typeof dimension.satisfied_outcomes === "number" ? (
        <p className="wa-score">完成产出 {dimension.satisfied_outcomes}/{dimension.counted_outcome_keys?.length ?? 0}</p>
      ) : null}
      {dimension.confidence ? (
        <p className="wa-level">置信度等级：{dimension.confidence.level}</p>
      ) : null}
      {dimension.counted_outcome_keys?.length ? (
        <ul className="dream-detail-list">
          {dimension.counted_outcome_keys.map((key, index) => (
            <li key={`counted-${key}-${index}`}><small>{key}</small></li>
          ))}
        </ul>
      ) : null}
      {dimension.evidence?.length ? (
        <ul className="dream-detail-list">
          {dimension.evidence.map((item, index) => (
            <li key={`evidence-${item.handle}-${index}`}><strong>{item.kind}</strong><small>{item.handle}</small></li>
          ))}
        </ul>
      ) : null}
      {dimension.blockers?.length ? (
        <ul className="dream-detail-list">
          {dimension.blockers.map((blocker, index) => (
            <li key={`blocker-${blocker.handle}-${index}`}><strong>{blocker.kind}</strong><small>{blocker.delay} · {blocker.confidence}</small></li>
          ))}
        </ul>
      ) : null}
    </section>
  );
}

function formatTime(value: string) {
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString("zh-CN");
}
