import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'
import { SettingsMutationProvider } from './settingsMutation'
import './styles.css'
import 'uplot/dist/uPlot.min.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <SettingsMutationProvider>
      <App />
    </SettingsMutationProvider>
  </StrictMode>,
)
