import { Link } from "react-router-dom";

export function NotFoundPage() {
  return (
    <main className="empty-page">
      <h1>页面不存在</h1>
      <Link to="/">返回总览</Link>
    </main>
  );
}
