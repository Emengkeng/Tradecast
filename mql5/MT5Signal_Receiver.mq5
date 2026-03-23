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
input bool   CopyModify     = true;
input bool   CopyClose      = true;
input long   Magic          = 88001;

string lastProcessedKey = ""; // ticket_id + ":" + signal_type

//+------------------------------------------------------------------+
int OnInit() {
   EventSetMillisecondTimer(PollIntervalMs);
   Print("[Receiver] Started. Watching: ", SymbolToWatch, " Magic: ", Magic);
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

   // Deduplicate
   string key = IntegerToString(ticketID) + ":" + signalType;
   if (key == lastProcessedKey) return;
   lastProcessedKey = key;

   Print("[Receiver] Got signal: ", signalType, " ", direction, " ", SymbolToWatch,
         " price=", price, " lot=", lot, " ticket=", ticketID);

   if      (signalType == "OPEN"                  ) OpenTrade(direction, sl, tp, lot, ticketID);
   else if (signalType == "MODIFY"  && CopyModify ) ModifyTrade(ticketID, sl, tp);
   else if (signalType == "CLOSE"   && CopyClose  ) CloseTrade(ticketID);
   else if (signalType == "PARTIAL" && CopyClose  ) PartialClose(ticketID, lot);
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