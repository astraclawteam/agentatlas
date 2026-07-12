import type { KnowledgeContent } from "../../api/changes";

export function ChangeDiff({ before, after }: { before: Partial<KnowledgeContent>; after: Partial<KnowledgeContent> }) {
  return (
    <div className="change-diff" aria-label="修改内容对比">
      <ReadableVersion label="修改前" content={before} />
      <ReadableVersion label="修改后" content={after} />
    </div>
  );
}

function ReadableVersion({ label, content }: { label: string; content: Partial<KnowledgeContent> }) {
  const steps = content.steps ?? [];
  const sections = content.sections ?? [];
  return (
    <section className="glass-rest change-version" aria-label={label}>
      <h2>{label}</h2>
      <dl>
        <div><dt>名称</dt><dd>{content.title || "未填写"}</dd></div>
        <div><dt>简要说明</dt><dd>{content.summary || "未填写"}</dd></div>
      </dl>
      {sections.map((section, index) => (
        <div className="change-readable-block" key={`${section.heading}-${index}`}>
          <strong>{section.heading || `第 ${index + 1} 部分`}</strong>
          <p>{section.body || "未填写"}</p>
        </div>
      ))}
      {steps.length ? (
        <ol className="change-readable-steps">
          {steps.map((step, index) => <li key={`${step.title}-${index}`}><strong>{step.title || `第 ${index + 1} 步`}</strong><p>{step.instruction || "未填写"}</p></li>)}
        </ol>
      ) : null}
    </section>
  );
}

