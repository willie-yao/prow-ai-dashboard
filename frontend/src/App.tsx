import { BrowserRouter, Routes, Route } from "react-router-dom";
import { DashboardPage } from "./pages/DashboardPage";
import { JobDetailPage } from "./pages/JobDetailPage";
import { TestDetailPage } from "./pages/TestDetailPage";
import { FlakinessPage } from "./pages/FlakinessPage";
import { Layout } from "./components/Layout";

export default function App() {
  return (
    <BrowserRouter basename="/capz-prow-dashboard">
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<DashboardPage />} />
          <Route path="flaky" element={<FlakinessPage />} />
          <Route path="job/:jobName" element={<JobDetailPage />} />
          <Route path="job/:jobName/test/:testName" element={<TestDetailPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  );
}
