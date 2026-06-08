// Icon shim: maps the Heroicons (react-icons/hi2) names the dashboard used onto
// their @mui/icons-material equivalents, in one place. This keeps existing JSX
// usages working after the react-icons removal and gives a single source of
// truth for the icon set. New code should import directly from
// "@mui/icons-material".
export {
  Search as HiMagnifyingGlass,
  ChevronRight as HiChevronRight,
  CheckCircle as HiCheckCircle,
  Cancel as HiXCircle,
  RemoveCircle as HiMinusCircle,
  AutoAwesome as HiSparkles,
  Assignment as HiClipboardDocumentList,
  Inventory2 as HiArchiveBox,
  Cloud as HiCloud,
  Dns as HiServerStack,
  Place as HiMapPin,
  SentimentSatisfiedAlt as HiFaceSmile,
} from "@mui/icons-material";
