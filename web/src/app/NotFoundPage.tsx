import { ButtonLink } from "./controls/Button";

export function NotFoundPage() {
  return (
    <main className="empty-page">
      <h1>页面不存在</h1>
      <ButtonLink to="/">返回总览</ButtonLink>
    </main>
  );
}
