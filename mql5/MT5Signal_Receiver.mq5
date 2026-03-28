//+------------------------------------------------------------------+
//| MT5Signal Receiver EA                                            |
//| Polls /pending/{symbol} and copies trades to this account.      |
//| Handles OPEN, MODIFY, CLOSE, PARTIAL signal types.              |
//+------------------------------------------------------------------+
#property copyright "MT4Signal"
#property version   "2.0"

input string ServerURL      = "http://your-vps-ip:8080/pending";
input string APIKey         = "your-api-key-here";
input string SymbolToWatch  = "EURUSD";
input long   AccountID      = 0;       // Set to your MT5 account number; 0 = auto
input int    PollIntervalMs = 300;
input double LotMultiplier  = 1.0;     // Scale lot size (1.0 = same as source)
input int    Slippage       = 3;       // In points
input bool   EnableModify   = true;
input bool   EnableClose    = true;
input long   Magic          = 88001;
input string StateFile = "mt5receiver_processed.csv";

// string lastProcessedKey = ""; // ticket_id + ":" + signal_type

struct ProcessedSignal {
   long   ticketID;
   string signalType;
   datetime processedAt;
};

ProcessedSignal processedSignals[];
int processedCount = 0;

//+------------------------------------------------------------------+
int OnInit() {
   LoadProcessedSignals();
   EventSetMillisecondTimer(PollIntervalMs);
   Print("[Receiver] Started. Watching: ", SymbolToWatch, " Magic: ", Magic);
   Print("[Receiver] Loaded ", processedCount, " processed signals from file.");
   return INIT_SUCCEEDED;
}

void OnDeinit(const int reason) {
   EventKillTimer();
}

void OnTimer() { PollAndAct(); }

//+------------------------------------------------------------------+
void PollAndAct() {
   string url     = ServerURL + "/" + SymbolToWatch;
   long   accID   = (AccountID > 0) ? AccountID : AccountInfoInteger(ACCOUNT_LOGIN);
   string headers = "X-API-Key: " + APIKey + "\r\n";
   headers       += "X-Account-ID: " + IntegerToString(accID);

   char   get[], result[];
   string resHeaders;

   int res = WebRequest("GET", url, headers, 3000, get, result, resHeaders);
   if (res == -1) {
      Print("[Receiver] WebRequest error ", GetLastError(),
            ". Whitelist ", url, " in Tools > Options > Expert Advisors.");
      return;
   }
   if (res == 204) return; // no signal
   if (res != 200) {
      Print("[Receiver] Server error ", res);
      return;
   }

   string json = CharArrayToString(result);
   if (json == "") return;

   // Parse fields
   long   ticketID   = JSONGetLong(json,   "ticket_id");
   string signalType = JSONGetString(json, "signal_type");
   string direction  = JSONGetString(json, "direction");
   double price      = JSONGetDouble(json, "price");
   double sl         = JSONGetDouble(json, "sl");
   double tp         = JSONGetDouble(json, "tp");
   double lot        = JSONGetDouble(json, "lot") * LotMultiplier;

   if (IsAlreadyProcessed(ticketID, signalType)) {
      return;  // Already handled this signal
   }

   Print("[Receiver] Got signal: ", signalType, " ", direction, " ", SymbolToWatch,
         " price=", price, " lot=", lot, " ticket=", ticketID);

   if      (signalType == "OPEN"                  ) OpenTrade(direction, sl, tp, lot, ticketID);
   else if (signalType == "MODIFY"  && EnableModify ) ModifyTrade(ticketID, sl, tp);
   else if (signalType == "CLOSE"   && EnableClose  ) CloseTrade(ticketID);
   else if (signalType == "PARTIAL" && EnableClose  ) PartialClose(ticketID, lot);
   
   // Mark as processed and save to file
   MarkAsProcessed(ticketID, signalType);
}

//+------------------------------------------------------------------+
void OpenTrade(string direction, double sl, double tp, double lot, long sourceTicket) {
   // Normalize lot to broker constraints
   double minLot  = SymbolInfoDouble(SymbolToWatch, SYMBOL_VOLUME_MIN);
   double maxLot  = SymbolInfoDouble(SymbolToWatch, SYMBOL_VOLUME_MAX);
   double lotStep = SymbolInfoDouble(SymbolToWatch, SYMBOL_VOLUME_STEP);
   lot = MathMax(minLot, MathMin(maxLot, MathRound(lot / lotStep) * lotStep));

   MqlTradeRequest req  = {};
   MqlTradeResult  resp = {};

   req.action      = TRADE_ACTION_DEAL;
   req.symbol      = SymbolToWatch;
   req.volume      = lot;
   req.type        = (direction == "BUY") ? ORDER_TYPE_BUY : ORDER_TYPE_SELL;
   req.price       = (req.type == ORDER_TYPE_BUY)
                        ? SymbolInfoDouble(SymbolToWatch, SYMBOL_ASK)
                        : SymbolInfoDouble(SymbolToWatch, SYMBOL_BID);
   req.sl          = sl;
   req.tp          = tp;
   req.deviation   = Slippage;
   req.magic       = Magic;
   req.comment     = "Copy#" + IntegerToString(sourceTicket);
   req.type_filling = ORDER_FILLING_IOC;

   if (!OrderSend(req, resp)) {
      Print("[Receiver] OrderSend failed: ", GetLastError(), " retcode=", resp.retcode);
   } else {
      Print("[Receiver] Trade opened. Ticket: ", resp.order);
   }
}

void ModifyTrade(long sourceTicket, double sl, double tp) {
   string comment = "Copy#" + IntegerToString(sourceTicket);

   for (int i = PositionsTotal() - 1; i >= 0; i--) {
      ulong ticket = PositionGetTicket(i);
      if (PositionGetInteger(POSITION_MAGIC) != Magic) continue;
      if (PositionGetString(POSITION_COMMENT) != comment) continue;

      MqlTradeRequest req  = {};
      MqlTradeResult  resp = {};

      req.action   = TRADE_ACTION_SLTP;
      req.symbol   = PositionGetString(POSITION_SYMBOL);
      req.sl       = sl;
      req.tp       = tp;
      req.position = ticket;

      if (!OrderSend(req, resp)) {
         Print("[Receiver] Modify failed: ", GetLastError(), " retcode=", resp.retcode);
      } else {
         Print("[Receiver] Trade modified. SL=", sl, " TP=", tp);
      }
      return;
   }
}

void CloseTrade(long sourceTicket) {
   string comment = "Copy#" + IntegerToString(sourceTicket);

   for (int i = PositionsTotal() - 1; i >= 0; i--) {
      ulong ticket = PositionGetTicket(i);
      if (PositionGetInteger(POSITION_MAGIC) != Magic) continue;
      if (PositionGetString(POSITION_COMMENT) != comment) continue;

      MqlTradeRequest req  = {};
      MqlTradeResult  resp = {};

      req.action      = TRADE_ACTION_DEAL;
      req.symbol      = PositionGetString(POSITION_SYMBOL);
      req.volume      = PositionGetDouble(POSITION_VOLUME);
      req.type        = (PositionGetInteger(POSITION_TYPE) == POSITION_TYPE_BUY)
                           ? ORDER_TYPE_SELL : ORDER_TYPE_BUY;
      req.price       = (req.type == ORDER_TYPE_SELL)
                           ? SymbolInfoDouble(req.symbol, SYMBOL_BID)
                           : SymbolInfoDouble(req.symbol, SYMBOL_ASK);
      req.deviation   = Slippage;
      req.magic       = Magic;
      req.position    = ticket;
      req.type_filling = ORDER_FILLING_IOC;

      if (!OrderSend(req, resp)) {
         Print("[Receiver] Close failed: ", GetLastError(), " retcode=", resp.retcode);
      } else {
         Print("[Receiver] Trade closed.");
      }
      return;
   }
}

void PartialClose(long sourceTicket, double newLot) {
   string comment = "Copy#" + IntegerToString(sourceTicket);

   for (int i = PositionsTotal() - 1; i >= 0; i--) {
      ulong ticket = PositionGetTicket(i);
      if (PositionGetInteger(POSITION_MAGIC) != Magic) continue;
      if (PositionGetString(POSITION_COMMENT) != comment) continue;

      double closeAmount = PositionGetDouble(POSITION_VOLUME) - newLot;
      if (closeAmount <= 0) return;

      double lotStep = SymbolInfoDouble(SymbolToWatch, SYMBOL_VOLUME_STEP);
      closeAmount = MathRound(closeAmount / lotStep) * lotStep;

      string sym = PositionGetString(POSITION_SYMBOL);

      MqlTradeRequest req  = {};
      MqlTradeResult  resp = {};

      req.action      = TRADE_ACTION_DEAL;
      req.symbol      = sym;
      req.volume      = closeAmount;
      req.type        = (PositionGetInteger(POSITION_TYPE) == POSITION_TYPE_BUY)
                           ? ORDER_TYPE_SELL : ORDER_TYPE_BUY;
      req.price       = (req.type == ORDER_TYPE_SELL)
                           ? SymbolInfoDouble(sym, SYMBOL_BID)
                           : SymbolInfoDouble(sym, SYMBOL_ASK);
      req.deviation   = Slippage;
      req.magic       = Magic;
      req.position    = ticket;
      req.type_filling = ORDER_FILLING_IOC;

      if (!OrderSend(req, resp)) {
         Print("[Receiver] Partial close failed: ", GetLastError(), " retcode=", resp.retcode);
      } else {
         Print("[Receiver] Partial close done. Closed ", closeAmount, " lots.");
      }
      return;
   }
}

//+------------------------------------------------------------------+
// Minimal JSON parsing helpers
long JSONGetLong(string json, string key) {
   return StringToInteger(JSONGetRaw(json, key));
}

double JSONGetDouble(string json, string key) {
   string val = JSONGetRaw(json, key);
   if (val == "" || val == "null") return 0;
   return StringToDouble(val);
}

string JSONGetString(string json, string key) {
   string search = "\"" + key + "\":\"";
   int start = StringFind(json, search);
   if (start < 0) return "";
   start += StringLen(search);
   int end = StringFind(json, "\"", start);
   if (end < 0) return "";
   return StringSubstr(json, start, end - start);
}

string JSONGetRaw(string json, string key) {
   string search = "\"" + key + "\":";
   int start = StringFind(json, search);
   if (start < 0) return "";
   start += StringLen(search);
   while (start < StringLen(json) && StringGetCharacter(json, start) == ' ') start++;
   int end = start;
   while (end < StringLen(json)) {
      ushort c = StringGetCharacter(json, end);
      if (c == ',' || c == '}') break;
      end++;
   }
   return StringSubstr(json, start, end - start);
}

//+------------------------------------------------------------------+
// File-based deduplication
//+------------------------------------------------------------------+
bool IsAlreadyProcessed(long ticketID, string signalType) {
   for (int i = 0; i < processedCount; i++) {
      if (processedSignals[i].ticketID == ticketID && 
          processedSignals[i].signalType == signalType) {
         return true;
      }
   }
   return false;
}

void MarkAsProcessed(long ticketID, string signalType) {
   ArrayResize(processedSignals, processedCount + 1);
   processedSignals[processedCount].ticketID = ticketID;
   processedSignals[processedCount].signalType = signalType;
   processedSignals[processedCount].processedAt = TimeCurrent();
   processedCount++;
   
   SaveProcessedSignals();
   
   // Optional: Clean up old entries (older than 24 hours)
   CleanupOldSignals();
}

void SaveProcessedSignals() {
   int handle = FileOpen(StateFile, FILE_WRITE | FILE_CSV | FILE_COMMON);
   if (handle == INVALID_HANDLE) {
      Print("[Receiver] Cannot open state file for writing: ", StateFile);
      return;
   }
   
   for (int i = 0; i < processedCount; i++) {
      FileWrite(handle,
         IntegerToString(processedSignals[i].ticketID),
         processedSignals[i].signalType,
         IntegerToString((long)processedSignals[i].processedAt)
      );
   }
   
   FileClose(handle);
}

void LoadProcessedSignals() {
   if (!FileIsExist(StateFile, FILE_COMMON)) {
      Print("[Receiver] State file not found - starting fresh.");
      return;
   }
   
   int handle = FileOpen(StateFile, FILE_READ | FILE_CSV | FILE_COMMON);
   if (handle == INVALID_HANDLE) {
      Print("[Receiver] Cannot open state file for reading: ", StateFile);
      return;
   }

   processedCount = 0;
   ArrayResize(processedSignals, 0);

   while (!FileIsEnding(handle)) {
      string ticketStr = FileReadString(handle);
      if (ticketStr == "") break;
      
      string signalType = FileReadString(handle);
      string timestampStr = FileReadString(handle);
      
      long ticketID = StringToInteger(ticketStr);
      datetime processedAt = (datetime)StringToInteger(timestampStr);
      
      ArrayResize(processedSignals, processedCount + 1);
      processedSignals[processedCount].ticketID = ticketID;
      processedSignals[processedCount].signalType = signalType;
      processedSignals[processedCount].processedAt = processedAt;
      processedCount++;
   }
   
   FileClose(handle);
}

void CleanupOldSignals() {
   // Remove signals older than 24 hours to prevent file bloat
   datetime cutoff = TimeCurrent() - (24 * 3600);
   int newCount = 0;
   
   for (int i = 0; i < processedCount; i++) {
      if (processedSignals[i].processedAt > cutoff) {
         if (newCount != i) {
            processedSignals[newCount] = processedSignals[i];
         }
         newCount++;
      }
   }
   
   if (newCount < processedCount) {
      processedCount = newCount;
      ArrayResize(processedSignals, processedCount);
      SaveProcessedSignals();
      Print("[Receiver] Cleaned up old signals. Now tracking ", processedCount, " signals.");
   }
}
